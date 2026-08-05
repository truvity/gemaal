# Changelog

One line per release; full detail lives in the release notes and the
git history.

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
