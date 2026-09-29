# Safety — every refusal, and what it prevents

Every rule below fails a config load, a render, or a request outright.
None of them retry, warn-and-continue, or silently narrow scope — a
refusal is the point.

## Config load (`pkg/config`)

| Refusal | Prevents |
|---|---|
| `awsRegion` unset | the SDK falling back to an environment variable EKS Pod Identity never sets, silently resolving to whatever region the SDK's own default chain lands on |
| an SSM root containing `/secrets` | the sweep ever touching the secrets tree, which has a different writer and a different lifecycle |
| an SSM root not under `/test/` | a sweeper pointed at anything that is not shaped like a test estate |
| an S3 bucket with no `test` segment | the same shape check for S3 |
| `identity.personalNamespace` without `{slug}` | a template that could render the same namespace for every person |
| an `identity.emails` key that is not an email, or a value that is not an RFC1123 label | a malformed identity map producing a namespace name Kubernetes would refuse at apply time, discovered mid-sweep instead of at load |
| `hold.default` exceeding `hold.max` | a default that is already out of its own bound |

An unknown top-level key in the config document is refused too
(`KnownFields(true)`): a typo in a deployment's values is a startup
error, not a silently ignored knob.

## The engine

### Tier-label containment

A namespace without the configured tier label does not exist as far as
gemaal is concerned. This is enforced by the selector, not by
convention: `gemaal-system` and every platform namespace are
structurally unreachable without someone deliberately labelling them.

### Shadow mode by default

`dryRun: true` (the chart's `confirm: false`): the housekeeping loop and
the `Sweep` RPC plan and report, never delete, until a deployer flips
the value. Every sweep — dry-run or not — leaves a structured deletion
record.

### Unreadable is held, not deleted

A release Secret whose ledger labels or timestamp cannot be parsed is
HELD with the problem recorded; planning continues for the rest of the
estate. Tick-level fatality is reserved for infrastructure failures (a
`helm`/`kubectl` exec or decode error), where the listing itself is
untrustworthy — never for one release's malformed data.

### RBAC is split

The chart's watcher RBAC (namespaces, secrets, TokenReview) is separate
from its sweep RBAC (delete rights), and the sweep half is off by
default (`rbac.sweep.enabled: false`). Observing is not deleting, and
the two are granted separately.

## Auth

- Workload callers authenticate by TokenReview against the cluster's own
  API — the API server, not this service, is the authority on what the
  token means.
- A person's token is verified against access-issuer's signing keys and
  audience (`authz.issuerURL`/`audience`), with access-roster's own
  identity package. An unset `issuerURL` means only TokenReview callers
  authenticate at all; nothing here trusts an unverified claim, because
  "only the gateway can reach this" is one NetworkPolicy edit or one
  port-forward away from false.
- `Checkout`/`Extend` require the caller to own the target namespace (an
  `emp:{slug}` group or resolved email that renders to it) or hold an
  admin group/user. `Sweep` is admin-only. An empty `authz.adminGroups`
  and `authz.adminUsers` means nobody is an admin — `Sweep` then refuses
  everyone, which is the safe default for a fresh deployment.

## What a compromised or malfunctioning gemaal can and cannot do

See [SECURITY.md](../SECURITY.md): the short version is that gemaal is a
watcher, not a deployer, so its blast radius is delayed cleanup, never a
workload it chose to run.
