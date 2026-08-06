# gemaal

> **Early development.** The design, the proto surface, the client
> library, the CLI and the service are in place; the service has not
> yet seen a production deployment (that is G4, shadow mode first).
> The client API is in real use (bar's url-shortener suite) but may
> still change between 0.x minors without a compatibility shim.

A *gemaal* is a Dutch pumping station — the machine that keeps a polder
dry. Land below sea level does not stay dry because water is forbidden
to enter; it stays dry because something never stops pumping it out.
A shared test cluster works the same way: installations happen freely,
all the time, by anyone — and what keeps the cluster habitable is not a
gate in front of installs but a pump behind them, watching, aging, and
draining what nobody is using anymore. gemaal is that pump.

gemaal **never installs anything**. Clients run `helm upgrade --install`
themselves; the service only ever uninstalls and sweeps. A down gemaal
delays cleanup — it never blocks anyone's loop.

## Three faces

| Face | What it is |
|---|---|
| **`gemaal`** (service) | in-cluster watcher: TTL housekeeping over test tenants, ring-pair-aware teardown, orphaned-artifact sweeps, the six ConnectRPC RPCs (Plan / ListTenants / Checkout / Extend / Sweep / Resolve), and the web console |
| **`gemaalctl`** (CLI) | `whoami` (the identity evidence chain + the resolved tenant), `install`/`uninstall` (client-side helm with the ledger labels stamped, ring-pair aware), ConnectRPC client for plan / checkout / extend, and the artifact pipeline |
| **Go library** | what test harnesses import: `pkg/harness` (resolve the standing tenant, bracket the suite's build/deploy/setup/teardown phases, install helpers), `pkg/identity` (evidence drivers + slug resolution, incl. the interim kubectl-groups resolver), `pkg/gemaalcfg` (the committed `gemaal.yaml`) |

## Status

| Phase | Contents | |
|---|---|---|
| G1 | scaffold: design doc, `proto/gemaal/v1`, service + CLI skeletons | ✅ |
| G2 | identity drivers + standing-tenant harness + `gemaal.yaml` + gemaalctl proper | ✅ |
| G3 | service v1: housekeeping loop, real RPCs, auth, web console, helm chart | ✅ |
| G4 | service image + chart publishing, first deployment (shadow mode first) | ⏳ |

## The service

The service is the pump: a `gocron` loop that, every tick, re-derives
the world from the cluster (level-triggered, no store) and applies the
garbage contract of [docs/design.md](docs/design.md):

- **reach** — namespaces selected by the tier LABEL
  (`tierLabel: tenancy.truvity.io/tier` by default), values keyed by
  the configured `tiers`. No label, no existence: `gemaal-system` and
  every platform namespace are structurally out of reach.
- **release truth** — `helm list` per namespace (exec; no client-go, no
  Helm SDK — the service sees exactly what an operator sees), releases
  grouped into ring pairs (`<rel>` + `<rel>-infra`), the ledger read
  off the release Secrets.
- **rules** — uniform TTL from last activity (tenant `ttl` label, else
  tier default) with `keep-until` precedence; teardown is ring-ordered
  (app before infra); orphaned `/test/<ns>/<rel>/` SSM subtrees are
  collected past grace. The S3 sweep is stubbed off pending the shared
  test bucket — a configured bucket shows up in every plan as a
  problem, on purpose.
- **shadow mode by default** — `dryRun: true` (the chart's
  `confirm: false`): the loop and the Sweep RPC plan, report and
  delete nothing until the deployer flips it deliberately. Every sweep
  leaves structured deletion records (log + in-memory history for the
  console).
- **auth** — mutations authenticate via TokenReview against the k8s API
  (workloads) falling back to the gateway-forwarded OIDC JWT (humans);
  Checkout/Extend are owner-or-admin (the `emp:{slug}` group or the
  resolved email must render to the target namespace), Sweep is
  admin-only. `Resolve` answers from the deployer-rendered email→slug
  map.
- **panel** — server-rendered console at `/` (tenants: ages, tiers,
  ledger, pending actions, Checkout/Extend forms) and `/sweeps`
  (history). No scripts at all, CSP-hardened, stateless same-origin
  guard on the posts; browser login belongs to the gateway
  (Envoy SecurityPolicy OIDC), the panel consumes the forwarded
  identity.

The helm chart lives in [charts/gemaal](charts/gemaal): namespace-agnostic,
service account for pod identity, config from values (tier TTLs,
allow-list, identity map, `confirm`), watcher RBAC split from the
off-by-default sweep RBAC. The service image must carry `helm` and
`kubectl` binaries next to the `gemaal` binary (the exec boundary is
deliberate); the image build ships with G4.

## The test harness

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

## Design

The tenancy model — tenant identity, tier labels, the
`gemaal.io/{ttl,keep-until,execution-id}` label ledger, uniform TTL, the
garbage contract, the driver families, and the safety rails — lives in
[docs/design.md](docs/design.md). Read that first; the code is its
shadow.

## Development

Toolchain via [devbox](https://www.jetify.com/devbox/) (+ direnv), tasks
via [just](https://just.systems/):

```bash
just check      # build + test + lint + vuln — what CI runs
just generate   # regenerate gen/ from proto/ (buf; output is committed)
just run        # run the service skeleton against config.example.yaml
```

## License

[MIT](LICENSE)
