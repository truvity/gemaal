# Changelog

One line per release; full detail lives in the release notes and the
git history.

## 0.0.x
- **0.0.1** — G1 scaffold: repo shape (devbox / just / lefthook /
  goreleaser, workflows with every third-party action pinned to a commit
  SHA), `docs/design.md` (the generic tenancy model: label ledger,
  uniform TTL, garbage contract, driver families, safety rails),
  `proto/gemaal/v1` with committed connect-go codegen, service skeleton
  (`cmd/gemaal`: config load + healthz + stub RPCs + no-op housekeeping
  tick), CLI skeleton (`cmd/gemaalctl`: version / plan / checkout /
  extend).
