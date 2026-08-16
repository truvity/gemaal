# Running the service

gemaal is a pump, not a gate. The operational consequence is the one worth
internalising: **a down gemaal delays cleanup and blocks nobody.** There is no
install path through it, so an outage degrades tidiness, never anyone's loop.
Page it accordingly.

## Shadow mode is the default, and deliberately so

`dryRun: true` (the chart's `confirm: false`). The housekeeping loop and the
`Sweep` RPC plan, report, and delete **nothing** until a deployer flips it.

Leave it in shadow through the first deployment and read the sweep records
before granting the delete. The records are the point of shadow mode: every
sweep leaves structured deletion records (log, plus in-memory history the
console renders at `/sweeps`), so what *would* have been deleted is inspectable
before anything is.

## What it can reach

Namespaces are selected by the tier **label** (`tenancy.truvity.io/tier` by
default), values keyed by the configured `tiers`. No label, no existence.

That is a containment property, not a convenience: `gemaal-system` and every
platform namespace are structurally out of reach, so a misconfigured tier
cannot walk into them. Widening reach means labelling a namespace, which is a
deliberate act by someone with the rights to do it.

## RBAC is split

The chart separates **watcher** RBAC from **sweep** RBAC, and the sweep half is
off by default. Granting the ability to observe is not granting the ability to
delete, and the split lets you run the watcher in production while the delete
permission is still being argued about.

## The image carries helm and kubectl

Deliberate. The service reads release truth by exec-ing `helm list` — no
client-go, no Helm SDK — so it sees exactly what an operator running the same
command sees. That exec boundary is why the image must ship those binaries
next to the `gemaal` binary.

If they are missing, the service cannot establish release truth and will not
be able to plan; that is the first thing to check when a deployment comes up
but plans are empty.

## The console

Server-rendered at `/` (tenants: ages, tiers, ledger, pending actions,
Checkout/Extend forms) and `/sweeps` (history). No scripts at all,
CSP-hardened, stateless same-origin guard on posts.

**Browser login is not gemaal's.** It belongs to the gateway (Envoy
`SecurityPolicy` OIDC); the panel consumes the forwarded identity. Do not add
an auth path here — the gateway is the one place it belongs.

## Auth on mutations

TokenReview against the k8s API (workloads), falling back to the
gateway-forwarded OIDC JWT (humans).

- `Checkout` / `Extend` — owner-or-admin: the `emp:{slug}` group or the
  resolved email must render to the target namespace.
- `Sweep` — admin only.
- `Resolve` — answers from the deployer-rendered email→slug map.

## Known stub

The S3 sweep is stubbed off pending the shared test bucket. A configured
bucket shows up in **every plan as a problem, on purpose** — a silent no-op
would let you believe objects were being collected when nothing is.
