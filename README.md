# gemaal

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

AI agents: start with **[AGENTS.md](AGENTS.md)** — the exhaustive gemaalctl
command surface and the rules for documenting it.

## Who it is for

A platform team running a shared, ephemeral-tenant test cluster —
personal `emp-{slug}` namespaces and per-run CI namespaces alike — where
installs happen constantly and nobody remembers to clean up. gemaal is
the housekeeping loop and the client-side tooling around it; it is not
an admission gate (nothing here blocks an install) and not a secrets or
identity system (it consumes access-issuer-verified identity, it does
not mint it). A team with no shared test cluster, or one where installs
are already centrally orchestrated, has no use for this repository.

## The model

Three faces, one tenancy model:

| Face | What it is |
|---|---|
| **`gemaal`** (service) | in-cluster watcher: TTL housekeeping over test tenants, ring-pair-aware teardown, orphaned-artifact sweeps, the six ConnectRPC RPCs (Plan / ListTenants / Checkout / Extend / Sweep / Resolve), and the web console |
| **`gemaalctl`** (CLI) | `whoami` (the identity evidence chain + the resolved tenant), `install`/`uninstall` (client-side helm with the ledger labels stamped, ring-pair aware), ConnectRPC client for plan / checkout / extend, and the artifact pipeline |
| **Go library** | what test harnesses import: `pkg/harness` (resolve the standing tenant, bracket the suite's build/deploy/setup/teardown phases, install helpers), `pkg/identity` (evidence drivers + slug resolution, incl. the interim kubectl-groups resolver), `pkg/gemaalcfg` (the committed `gemaal.yaml`) |

The service re-derives the world from the cluster every tick
(level-triggered, no store) and applies the garbage contract of
[docs/design.md](docs/design.md):

- **reach** — namespaces selected by the tier LABEL (`tierLabel:
  tenancy.truvity.io/tier` by default), values keyed by the configured
  `tiers`. No label, no existence: `gemaal-system` and every platform
  namespace are structurally out of reach.
- **release truth** — `helm list` per namespace (exec; no client-go, no
  Helm SDK), releases grouped into ring pairs (`<rel>` + `<rel>-infra`),
  the ledger read off the release Secrets.
- **rules** — uniform TTL from last activity with `keep-until`
  precedence; teardown is ring-ordered (app before infra); orphaned
  `/test/<ns>/<rel>/` SSM subtrees are collected past grace. The S3
  sweep is stubbed off pending the shared test bucket.
- **shadow mode by default** — `dryRun: true` (the chart's `confirm:
  false`): the loop and the Sweep RPC plan, report and delete nothing
  until the deployer flips it deliberately.
- **auth** — mutations authenticate via TokenReview against the k8s API
  (workloads), falling back to a person's token verified against
  access-issuer's keys with access-roster's identity package
  (`authz.issuerURL`/`audience`); a token neither authority vouches for
  is refused. Checkout/Extend are owner-or-admin, Sweep is admin-only.
- **console** — the web console at `/` (Vite/React/MUI single-page app
  over Connect-Web): Tenants (ages, tiers, ledger, pending actions,
  Checkout/Extend/Decommission) and Sweeps (history). Browser sign-in is
  gateway-owned (Envoy Gateway's `SecurityPolicy` OIDC filter, not a
  proxy this repository runs); the console consumes the forwarded
  identity and renders access-roster's `UserBadge`.

## Install and a worked example

```bash
helm install gemaal oci://ghcr.io/truvity/charts/gemaal --version 0.24.3 \
  --set confirm=false
```

Pin a real version — an OCI chart reference has no `@latest` tag to fall
back to anyway, and a floating pin is exactly the drift this contract
forbids. Deploy in shadow mode first (`confirm=false`, the chart
default) and read the sweep records at `/sweeps` before granting
deletion.

Two values are the chart's own product surface, not an estate fact
supplied by the deployer — they carry defaults because gemaal defines
them, the way an API defines its own default port:

- `config.identity.personalNamespace` (`emp-{slug}`) — the template a
  resolved slug's standing namespace renders from.
- `config.tierLabel` (`tenancy.truvity.io/tier`) — the namespace label
  gemaal's own reach selector reads.

Every other cluster-specific value — `config.awsRegion`, the tier TTLs,
the allow-listed S3 buckets and SSM roots, the identity map — is
**required, with no default**: an estate fact the deployer states
explicitly. `config.awsRegion` in particular has no fallback to
`AWS_REGION` from the environment either — EKS Pod Identity never
injects it, so config load refuses to start rather than guess. See
[config.example.yaml](config.example.yaml) for the full schema and
[docs/adoption.md](docs/adoption.md) for install order and prerequisites.

### The test harness

A project's integration tests resolve their standing tenant once, in
TestMain, and run inside it — the harness itself creates and deletes
nothing (cleanup is the service's job, driven by the labels installs
stamp). A real suite brackets `m.Run()` with phases, and `harness.Run`
owns the bracket:

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
			App:         "example-app",
			Config:      cfg,
		},

		// Build the packaged charts through the repo-owned hook.
		Build: func(ctx context.Context, _ *harness.Cluster, _ harness.Tenant) error {
			return runHook(ctx, cfg.BuildHook("example-app"))
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

The full `GEMAAL_TEST_*` contract, the resolution ladders, and the kind
tier for public repos without shared-cluster routing are in
[docs/reference.md](docs/reference.md); running a suite in CI
(one identity per phase, the teardown guarantee) is in
[docs/harness-ci.md](docs/harness-ci.md).

## Consumers

| Consumer | Surface |
|---|---|
| `truvity/gitops` | the chart, Kargo-promoted |
| `truvity/policy` | its example e2e harness, Go `harness` |

## Neighbours

- **ocictl ↔ gemaal**: gemaal's pipeline shells out to `helmctl`
  (`go tool helmctl`, from `truvity/ocictl`) to package and push charts.

## Documentation

- [docs/adoption.md](docs/adoption.md): prerequisites, install order,
  and the shadow-mode-first rollout
- [docs/safety.md](docs/safety.md): every refusal — in `pkg/config` and
  in the engine — and the failure it prevents
- [docs/reference.md](docs/reference.md): the `gemaalctl` command
  surface, the `GEMAAL_TEST_*` contract, and the resolution ladders
- [docs/doctrine.md](docs/doctrine.md): what this repository owns and
  what the consuming estate owns
- [docs/design.md](docs/design.md): the tenancy model in full — label
  ledger, uniform TTL, the garbage contract, driver families
- [docs/harness-ci.md](docs/harness-ci.md): running a harness suite in
  CI — identity per phase, the teardown guarantee, release-derived
  chart values
- [CHANGELOG.md](CHANGELOG.md): what changed for a consumer, per
  version

## The rule that makes this repository public

**Mechanism only.** Nothing here names a real account, cluster, hostname
or secret path — every such thing is a config input with no default
(see [Install and a worked example](#install-and-a-worked-example)), and
the deploying estate supplies it from its own (private) repository.
`hack/leak-canary.sh` enforces this in CI, and public history cannot be
unpublished — so the rule is mechanical, not remembered.

This repository follows the shared [component
contract](https://github.com/truvity/policy/blob/master/docs/contracts/component.md).

## Status

| Phase | Contents | |
|---|---|---|
| G1 | scaffold: design doc, `proto/gemaal/v1`, service + CLI skeletons | ✅ |
| G2 | identity drivers + standing-tenant harness + `gemaal.yaml` + gemaalctl proper | ✅ |
| G3 | service v1: housekeeping loop, real RPCs, auth, web console, helm chart | ✅ |
| G4 | service image + chart publishing | ✅ |

`ghcr.io/truvity/gemaal/server` and `oci://ghcr.io/truvity/charts/gemaal`
publish on every tag; see [Releases](https://github.com/truvity/gemaal/releases)
for the current version.

## Development

Toolchain via [devbox](https://www.jetify.com/devbox/) (+ direnv), tasks
via [just](https://just.systems/):

```bash
just check      # build + test + lint + chart-lint + vuln + leak-canary — what CI runs
just generate   # regenerate gen/ from proto/ (buf; output is committed)
just run        # run the service skeleton against config.example.yaml
```

## Releasing

Push a tag `vX.Y.Z`. The shared release workflow (`truvity/ci-workflows`
`release-public.yaml`) builds and publishes the `gemaal`/`gemaalctl`
binaries, the service image (`ghcr.io/truvity/gemaal/server`, via `ko`)
and the chart (`oci://ghcr.io/truvity/charts/gemaal`), all stamped from
the tag.

Auto-release is armed (`vars.AUTO_RELEASE=true`): a merged pull request
labelled `security` releases immediately, and Monday's cron cuts a patch
for whatever renovate bumped in the meantime. Minors and majors are
still cut by hand, tagged after their CHANGELOG heading lands.

## Licence

[MIT](LICENSE)
