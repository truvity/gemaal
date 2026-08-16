<!-- Moved out of README.md unchanged; the README now links here. -->

Consuming gemaal from a project's integration tests. The command surface and
the `GEMAAL_TEST_*` dials are in [the CLI reference](../reference/cli.md).

# The test harness

A project's integration tests resolve their standing tenant once, in
TestMain, and run inside it — the harness itself creates and deletes
nothing (cleanup is the service's job, driven by the labels installs
stamp). A real suite brackets `m.Run()` with phases, and `harness.Run`
owns the bracket — resolution, the `GEMAAL_*` export, skip-flag
semantics, signal handling, teardown-even-on-partial-install:

```go
func TestMain(m *testing.M) {
    flag.Parse()
    if testing.Short() {
        return // unit-only run
    }

    cfg, err := gemaalcfg.Load("gemaal.yaml")
    if err != nil {
        log.Fatal(err)
    }

    os.Exit(harness.Run(m, harness.Suite{
        Options: harness.Options{
            Kubecontext: "devel@oidc",
            App:         "url-shortener",
            Config:      cfg,
        },

        // Build the packaged charts through the repo-owned hook.
        Build: func(ctx context.Context, _ *harness.Cluster, _ harness.Tenant) error {
            return runHook(ctx, cfg.BuildHook("url-shortener"))
        },

        // Install the ring pair with the ledger stamped.
        Deploy: func(ctx context.Context, c *harness.Cluster, t harness.Tenant) error {
            return c.InstallPair(ctx, t, harness.Pair{
                InfraChart: infraTgz, AppChart: appTgz,
                Labels: harness.Labels{ExecutionID: harness.DefaultExecutionID(time.Now())},
            })
        },

        // Never skipped: a reused install still has to be ready.
        Setup: []harness.Hook{func(ctx context.Context, c *harness.Cluster, t harness.Tenant) error {
            if err := c.WaitForDeployments(ctx, t.Namespace, t.Release, harness.InfraRelease(t.Release)); err != nil {
                return err
            }

            url, err := c.ServiceURL(ctx, t.Namespace, t.Release+"-web", 8080)
            if err != nil {
                return err
            }

            return harness.WaitHTTPReady(ctx, url, time.Minute)
        }},

        // Interim, until the service's TTL housekeeping owns cleanup.
        Teardown: []harness.Hook{func(ctx context.Context, c *harness.Cluster, t harness.Tenant) error {
            return c.UninstallPair(ctx, t)
        }},
    }))
}
```

The operator's dials are the `GEMAAL_TEST_*` contract (strict booleans —
a set-but-unparseable value refuses the run instead of silently
destroying what it was told to keep):

| Variable | Effect |
|---|---|
| `GEMAAL_TEST_SKIP_BUILD` | skip `Build`; use the artifacts already lying around |
| `GEMAAL_TEST_SKIP_DEPLOY` | skip `Build` and `Deploy`; reuse the standing releases as installed |
| `GEMAAL_TEST_SKIP_DESTROY` / `GEMAAL_TEST_KEEP` | skip `Teardown`; keep the releases after the run |

Resolution ladders (each rung explicit, first hit wins):

- **namespace**: `Options.Namespace` → `GEMAAL_NAMESPACE` → identity
  chain (`GEMAAL_EMAIL` → `kubectl auth whoami` → AWS SSO session) →
  email → slug → the `personalNamespace` template (`emp-{slug}`)
- **release**: `Options.Release` → `GEMAAL_RELEASE` →
  CI (`r{run}-a{attempt}`) → `Options.App`
- **slug**: `Options.Resolver` → the `gemaal.yaml` identity map → the
  interim kubectl-groups resolver (the `emp:{slug}` group in the
  caller's own cluster token — no committed people data; the service's
  Resolve RPC stays the end state)

The resolved pair is exported as `GEMAAL_NAMESPACE` / `GEMAAL_RELEASE` /
`GEMAAL_KUBECONTEXT`. Installs go through `harness.Cluster`: helm ≥ 3.13
`--labels` stamping (`gemaal.io/{ttl,keep-until,execution-id}`),
ring-pair ordering (`<rel>-infra` installs first, uninstalls last),
rollout waits and Service ClusterIP resolution.
`harness.TierForNamespace` derives the client-side tier from the
namespace-name convention (`emp-` → employee, `ci-` → ci) for charts
that want it as a value. `gemaal.example.yaml` documents the committed
per-repo configuration.

