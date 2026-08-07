# Changelog

One line per release; full detail lives in the release notes and the
git history.

## 0.4.x
- **0.4.2** — chart-only: the egress NetworkPolicy admits the EKS Pod
  Identity credential endpoint (169.254.170.23:80, the node's
  link-local agent, cleartext HTTP). 0.4.1 on devel proved the gap
  empirically: with the PodIdentityAssociation live the SDK finally had
  a credential source, but the DNS+443-only egress dropped the fetch,
  so the first SSM call spent ~4.5 minutes in dial timeouts before the
  tick surfaced the credential error as the /test/ root's problem. No
  Go changes.
- **0.4.1** — the SSM sweep actually reaches AWS: the deployed client
  could never resolve a region — EKS Pod Identity injects only the
  credential-endpoint variables (never AWS_REGION), IMDS is unreachable
  from pods, and the chart sets no region — so every housekeeping tick
  reported the allow-listed /test/ root as a problem. The region is now
  explicit-with-a-default, resolved at config load in one visible place
  (document > AWS_REGION > `eu-central-1`), so the SDK's environment
  fallback never engages. `AWSSSMClient.API` narrows from `*ssm.Client`
  to the new `SSMAPI` interface (the SDK's own paginator interface plus
  DeleteParameters) and gains unit tests against a stubbed API: request
  shaping, page-token threading, incomplete-row skips, delete batches
  of 10, and the out-of-root refusal. aws-sdk-go-v2 modules are recorded
  as the direct dependencies they always were. The engine's SSMClient
  port, the sweep rules, and the chart are untouched.
- **0.4.0** — the Resolve feed (begins the staged emp:{slug} claim
  retirement): `identity.ResolveClient` — a ConnectRPC client speaking
  the deployed service's `gemaal.v1` Resolve RPC (short timeout, hard
  errors naming the authority) — slots into the resolver ladder as the
  rung before the interim kubectl-groups fallback. The ladder is now:
  explicit resolver → gemaal.yaml identity map → service Resolve RPC
  (when a server is configured: `GEMAAL_SERVER`, else gemaal.yaml's new
  `server` base-URL field) → `KubectlGroupsResolver` (interim,
  unchanged). The service side is untouched: it has served Resolve from
  the deployer-rendered `config.identity.emails` map since 0.3.0.

## 0.3.x
- **0.3.2** — helm v4 timestamp fix, found by shadow mode on devel: v4
  release Secrets serialize "updated" with the numeric offset repeated
  ("+0200 +0200") where v3 wrote the zone name ("+0200 CEST"); the
  parser now accepts that layout, and — the deeper flaw — a per-release
  unparseable timestamp no longer aborts the whole housekeeping tick:
  the release's tenant is HELD with the problem recorded (never delete
  what you cannot read, same rule as unparseable ledger labels) and
  planning continues for the rest of the estate. Tick-level fatality is
  reserved for infrastructure failures (helm/kubectl exec or decode
  errors), where the listing itself is untrustworthy.
- **0.3.1 (unreleased)** — release plumbing for G4: the service image
  (ko, `ghcr.io/truvity/gemaal`, base `alpine/k8s:1.33.4` so `helm` and
  `kubectl` ship beside the binary, amd64+arm64) and the chart pushed to
  `oci://ghcr.io/truvity/charts/gemaal` — v0.3.0 published archives
  only; chart sets `HOME=/home/nonroot` so helm/kubectl caches land on
  the writable emptyDir under the read-only rootfs.
- **0.3.0 (unreleased)** — G3 service v1: `pkg/engine` (the housekeeping
  engine — the bar#486 janitor prototype ported to the label-ledger
  model: plan/apply split, exec helm/kubectl ports, uniform TTL from
  last activity with keep-until precedence, tier selection by namespace
  LABEL, ring-ordered teardown, SSM orphan sweep, S3 stubbed off pending
  the bucket, re-authorization of every action, structured deletion
  records + bounded history); `pkg/authn` (TokenReview via the k8s API —
  no client-go — chained with gateway-forwarded JWT claims, failing
  closed); the six RPCs implemented (Checkout/Extend stamp keep-until by
  patching the release Secrets' labels, owner-or-admin; Sweep
  admin-only, server dry-run wins; Resolve from the deployer-rendered
  map); `pkg/panel` (roster-stack console: tenants + sweep history,
  CSP-hardened, no scripts, stateless same-origin guard);
  `charts/gemaal` (config from values, `confirm: false` = shadow mode,
  watcher RBAC split from off-by-default sweep RBAC). Shadow mode is the
  default everywhere: a fresh deployment plans and deletes nothing.

## 0.2.x
- **0.2.0 (unreleased)** — API-friction pass from bar's adoption
  (bar#488): `identity.KubectlGroupsResolver` promoted from bar's
  interim pattern (slug from the caller's own `emp:{slug}` token group;
  prefix configurable; the sanctioned interim until the service Resolve
  RPC) and wired as the resolver ladder's last rung — gemaalctl
  whoami/install now resolve without any identity config file;
  `harness.Run` redesigned around `Suite` phases (Build → Deploy →
  Setup → tests → Teardown) with the library-owned `GEMAAL_TEST_*`
  skip contract (strict booleans); `harness.TierForNamespace`
  (`emp-` → employee, `ci-` → ci, else unknown); whoami prints the
  tier. Charts adopting `.Values.gemaal` stays a consumer-side
  migration — accepted, see docs/design.md.

## 0.1.x
- **0.1.0** — first tagged release: the G1+G2 surface below, adopted by
  bar's url-shortener suite (bar#488).

## 0.0.x
- **0.0.2** — G2 harness + identity: `pkg/identity` (evidence drivers —
  env / kubectl / aws — chained with clear precedence and a recorded
  trail; email→slug resolver seam with the gemaal.yaml map as the first
  source), `pkg/gemaalcfg` (`gemaal.yaml`: cluster values as
  `.Values.gemaal`, build hooks, personal-namespace template, identity
  map), `pkg/harness` (standing-tenant resolution ladders with GEMAAL_*
  env contract and 63/53 bounds, `Run(m)` for TestMain, helm install
  helpers with `gemaal.io/{ttl,keep-until,execution-id}` label stamping,
  ring-pair ordering, rollout waits, Service ClusterIP resolution),
  gemaalctl `whoami` / `install` / `uninstall`. Fresh GEMAAL_* naming —
  no FLEET_* compatibility; the ephemeral harness mode was deliberately
  not ported.
- **0.0.1** — G1 scaffold: repo shape (devbox / just / lefthook /
  goreleaser, workflows with every third-party action pinned to a commit
  SHA), `docs/design.md` (the generic tenancy model: label ledger,
  uniform TTL, garbage contract, driver families, safety rails),
  `proto/gemaal/v1` with committed connect-go codegen, service skeleton
  (`cmd/gemaal`: config load + healthz + stub RPCs + no-op housekeeping
  tick), CLI skeleton (`cmd/gemaalctl`: version / plan / checkout /
  extend).
