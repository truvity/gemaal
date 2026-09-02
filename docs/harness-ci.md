# Running a harness suite in CI

What `pkg/harness` deliberately does NOT decide for you, learned the
expensive way by its first two adopters (bar's url-shortener/eudi
suites, core's dms suite — see their Integration workflows for living
reference). The library brackets Build → Deploy → Setup → tests →
Teardown; everything below is the caller's side of the contract.

## One identity per phase

The Build phase and the test phase want DIFFERENT credentials, and
running them under one identity fails in ways that point everywhere but
here:

| phase | needs | identity |
|---|---|---|
| Build (`pipeline snapshot`) | ECR push, CodeArtifact read, go-build-cache **write** | the runner pool's ambient pod identity |
| suite (`go test`) | the project's test resources (S3/KMS/...) | the test profile (Zitadel identity) |

The test profile deliberately holds none of the pool's infrastructure
grants ("infrastructure is the pool's business" — gitops managed
policies). Symptoms of getting this wrong, as observed:

- go-build-cache writes fail in batches (`put_s3_error` in the
  GOCACHE_METRICS lines) for every `go` invocation under the test
  profile, while invocations before the profile export succeed — the
  cache silently never warms;
- an in-container `yarn install` dies on registry auth minutes into a
  build, forty log lines from the missing token.

So: **run the Build in its own workflow step** under the ambient
identity, with `GEMAAL_AWS_AMBIENT=1` (drops the config's committed
profiles) — then run the suite with `GEMAAL_TEST_SKIP_BUILD=1`,
reusing `dist/`. The suite step exports its test profile without
poisoning the build.

## The teardown guarantee is yours, not the library's

`Suite.Teardown` runs after `m.Run()` returns. A TestMain body that
calls `os.Exit` on failure — go-core's `maincommon.Main` does — skips
it **by construction**. Locally that is the standing-tenant model
working as intended (a kept pair is upgraded in place by the next run).
In CI it is a leak: pair per red run, CNPG cluster included.

The pattern: a shell `trap` in the recipe that `helm uninstall
--ignore-not-found`s both releases on EXIT. Idempotent against the
harness's own teardown on the green path, and it still runs when the
suite exits early. A SIGKILLed job (cancellation) skips even the trap —
pair a stale-release sweep with it (age-gated, matching only your
per-run release names).

## Release-derived chart values are the caller's duty

`DeployPair` passes your `Set` values verbatim. Anything a chart
derives from the release name — CNPG cluster names, Services, secret
names — must be derived HERE from `tenant.Release`, because the chart's
defaults can only ever match one release name:

```go
"pg.clusterName=" + tenant.Release + "-pg",
```

A standing release named after the app masks the omission; the first
per-run CI release (`{app}-r{run}-a{attempt}`) finds it. A chart-side
guard that fails the render with the expected value in the message
(core's `pg-guard.yaml`) turns that from a debugging session into a
one-line fix.

## The lease is a feature

Two suites claiming one tenant/release contend on the gemaal lease; the
loser prints who holds it and stops. Per-run release names in CI make
contention impossible; locally, a run within ~90s of an interrupted one
waits out the abandoned claim. Do not "fix" this with retries.
