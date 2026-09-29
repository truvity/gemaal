# Reference

Read alongside [`cmd/gemaalctl/main.go`](../cmd/gemaalctl/main.go) and
[`cmd/gemaalctl/tenant.go`](../cmd/gemaalctl/tenant.go) — if this page
and those disagree, the source is right.

## `gemaalctl` commands

| Command | Does |
|---|---|
| `version` | build version |
| `whoami` | the identity evidence chain and the tenant it resolves to |
| `install` | client-side `helm upgrade --install` of the ring pair, ledger labels stamped (`--chart`, `--infra-chart`, `--ttl`, `--keep-until`) |
| `uninstall` | uninstall the ring pair — `<release>` first, then `<release>-infra` |
| `decommission` | uninstall the resolved tenant's ring pair **now**: the explicit end-of-life call |
| `plan` | ConnectRPC: what the service would do (`--namespace`, `--release` narrow it) |
| `checkout` | ConnectRPC: claim a tenant for a period (`--namespace`, `--release`, `--for`) |
| `extend` | ConnectRPC: push the keep-until out (`--namespace`, `--release`, `--for`) |
| `pipeline snapshot` | dev-loop artifact build — preview registry, no gates, working tree as-is |
| `pipeline push-preview` | push packaged ring charts to the preview registry |
| `pipeline release-stable` | gated stable release — five gates, then build + push in one run |

`plan`, `checkout`, `extend` and `pipeline` all need `--server`
(`GEMAAL_SERVER`) or `--config` (`GEMAAL_PIPELINE_CONFIG`) respectively.
**There is no `gemaalctl up`.** The tenant is *resolved* — from the
identity evidence chain — never passed in, so there is no `--project` or
`--suffix` either; the nearest command to a `... up` invocation from
another tool's CLI is `install`.

## The `GEMAAL_TEST_*` contract

Strict booleans: a set-but-unparseable value **refuses the run** rather
than silently destroying what it was told to keep.

| Variable | Effect |
|---|---|
| `GEMAAL_TEST_SKIP_BUILD` | skip `Build`; use the artifacts already lying around |
| `GEMAAL_TEST_SKIP_DEPLOY` | skip `Build` and `Deploy`; reuse the standing releases as installed |
| `GEMAAL_TEST_SKIP_DESTROY` / `GEMAAL_TEST_KEEP` | skip `Teardown`; keep the releases after the run |

## Resolution ladders

Each rung explicit, first hit wins.

**Namespace**
`Options.Namespace` → `GEMAAL_NAMESPACE` → identity chain (`GEMAAL_EMAIL`
→ `kubectl auth whoami` → AWS SSO session) → email → slug → the
`personalNamespace` template (`emp-{slug}` by default)

**Release**
`Options.Release` → `GEMAAL_RELEASE` → CI (`r{run}-a{attempt}`) →
`Options.App`

**Slug**
`Options.Resolver` → the `gemaal.yaml` identity map → the interim
kubectl-groups resolver (the `emp:{slug}` group in the caller's own
cluster token — no committed people data; the service's `Resolve` RPC
is the end state)

The resolved triple is exported as `GEMAAL_NAMESPACE`, `GEMAAL_RELEASE`
and `GEMAAL_KUBECONTEXT`. Installs go through `harness.Cluster`: helm
≥ 3.13 `--labels` stamping (`gemaal.io/{ttl,keep-until,execution-id}`
by default, following `config.labelDomain`), ring-pair ordering
(`<rel>-infra` installs first, uninstalls last), rollout waits and
Service ClusterIP resolution. `harness.TierForNamespace` derives the
client-side tier from the namespace-name convention (`emp-` →
employee, `ci-` → ci) for charts that want it as a value.
`gemaal.example.yaml` documents the committed per-repo configuration.

## The kind tier

Public repositories run the same suite against a disposable
[kind](https://kind.sigs.k8s.io/) cluster on a CI runner or a laptop,
where the shared cluster's service CIDR either is not routed or (Docker
Desktop on macOS) cannot be routed to at all. The suite code is
identical on both tiers; only the harness decides how to reach things,
via `harness.DetectTier` — a DIFFERENT axis from `TierForNamespace`'s
namespace-name convention, answering "which kind of cluster is this run
talking to" rather than classifying a name:

- **Detection**: `GEMAAL_TIER=kind` (explicit — what CI sets), or,
  failing that, a `Cluster.Kubecontext` named `kind-*` (kind's own
  convention). `Cluster.Tier` overrides both when a caller sets it
  directly.
- **Namespace**: fixed or configurable, same as every other tier — set
  `Options.Namespace` or `GEMAAL_NAMESPACE`, which already skip identity
  resolution entirely.
- **`ServiceURL`** opens a `kubectl port-forward` straight to the Pod
  behind the Service and returns `http://127.0.0.1:<local port>` instead
  of dialing the ClusterIP directly. The forward is tracked on the
  `*Cluster` and stopped by `(*Cluster).CloseForwards` — call it from
  your own `t.Cleanup`, or let `harness.Run` close it automatically
  after `m.Run()`.
- **`harness.DeployApp`** installs ring 3 alone — no ring2 pair — for
  the kind tier's lane, where the project's own infrastructure chart is
  never installed; a fixture substitutes for it.
- **Leases** (tenant claims) are unchanged: `coordination.k8s.io`
  objects reached the same way as every other kubectl call, no
  dependency on service-CIDR routing.

## `pkg/config` fields

See [config.example.yaml](../config.example.yaml) for the full document
with comments, and [safety.md](safety.md) for what each validation
refuses. `awsRegion`, the tier TTLs, `allowList.s3Buckets`/`ssmRoots`,
and `identity.emails` carry no default — every other field does; see
[adoption.md](adoption.md#defaults-that-are-gemaals-own-product-surface)
for which of those defaults are gemaal's own API surface rather than an
estate fact.
