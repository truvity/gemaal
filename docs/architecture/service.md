<!-- Moved out of README.md unchanged; the README now links here. -->
# The service — the pump

The service is the pump: a `gocron` loop that, every tick, re-derives
the world from the cluster (level-triggered, no store) and applies the
garbage contract of [the tenancy model](../design.md):

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

The helm chart lives in [charts/gemaal](../../charts/gemaal): namespace-agnostic,
service account for pod identity, config from values (tier TTLs,
allow-list, identity map, `confirm`), watcher RBAC split from the
off-by-default sweep RBAC. The service image must carry `helm` and
`kubectl` binaries next to the `gemaal` binary (the exec boundary is
deliberate); the image build ships with G4.

