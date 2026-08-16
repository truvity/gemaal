# gemaalctl reference

The human/CI face of gemaal. Read from
[`cmd/gemaalctl/main.go`](../../cmd/gemaalctl/main.go) and
[`cmd/gemaalctl/tenant.go`](../../cmd/gemaalctl/tenant.go) — if this page and
those disagree, they are right.

## Commands

| Command | Does |
| --- | --- |
| `version` | build version |
| `whoami` | the identity evidence chain and the tenant it resolves to |
| `install` | client-side `helm upgrade --install` of the ring pair, ledger labels stamped |
| `uninstall` | uninstall the ring pair — `<release>` first, then `<release>-infra` |
| `decommission` | uninstall the resolved tenant's ring pair **now**: the explicit end-of-life call |
| `plan` | ConnectRPC: what the service would do |
| `checkout` | ConnectRPC: claim a tenant for a period |
| `extend` | ConnectRPC: push the keep-until out |
| `pipeline snapshot` | artifact pipeline |
| `pipeline push-preview` | artifact pipeline |
| `pipeline release-stable` | artifact pipeline |

`install` takes `--chart`, `--infra-chart`, `--ttl`, `--keep-until`.
`plan`, `checkout` and `extend` take `--namespace` and `--release`;
`checkout` and `extend` also take `--for`.

**There is no `up`.** The tenant is *resolved* — from the identity evidence
chain — never passed in, so there is no `--project` or `--suffix` either.
Anything migrating from the retired `barctl test … up` wants `install`.

## The `GEMAAL_TEST_*` contract

Strict booleans: a set-but-unparseable value **refuses the run** rather than
silently destroying what it was told to keep.

| Variable | Effect |
| --- | --- |
| `GEMAAL_TEST_SKIP_BUILD` | skip `Build`; use the artifacts already lying around |
| `GEMAAL_TEST_SKIP_DEPLOY` | skip `Build` and `Deploy`; reuse the standing releases as installed |
| `GEMAAL_TEST_SKIP_DESTROY` / `GEMAAL_TEST_KEEP` | skip `Teardown`; keep the releases after the run |

## Resolution ladders

Each rung explicit, first hit wins.

**Namespace**
`Options.Namespace` → `GEMAAL_NAMESPACE` → identity chain (`GEMAAL_EMAIL` →
`kubectl auth whoami` → AWS SSO session) → email → slug → the
`personalNamespace` template (`emp-{slug}`)

**Release**
`Options.Release` → `GEMAAL_RELEASE` → CI (`r{run}-a{attempt}`) → `Options.App`

**Slug**
`Options.Resolver` → the `gemaal.yaml` identity map → the interim
kubectl-groups resolver (the `emp:{slug}` group in the caller's own cluster
token — no committed people data; the service's `Resolve` RPC is the end state)

The resolved triple is exported as `GEMAAL_NAMESPACE`, `GEMAAL_RELEASE` and
`GEMAAL_KUBECONTEXT`.
