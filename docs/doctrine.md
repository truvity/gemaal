# Doctrine — the design rules

The checkable rules every public component repository is held to —
chart version pinning, `values.schema.json`, golden renders, the leak
canary, the `CHANGELOG.md` shape, and the rest — now live in one place:
`truvity/policy`'s [component
contract](https://github.com/truvity/policy/blob/master/docs/contracts/component.md).
This repository follows it; the rules are not restated here.

What is gemaal-specific enough to state here instead:

## Watcher, not deployer

gemaal never installs anything, in the service or the CLI's `pipeline`
flows. Clients run `helm upgrade --install` themselves; the service's
only destructive verbs are uninstalling expired releases and sweeping
orphaned artifacts. This is a load-bearing property, not a preference:
a down or misbehaving gemaal delays cleanup and blocks no one's install
loop, and the ownership contract below follows from it.

| This repository | The deploying estate |
|---|---|
| the housekeeping engine, the RPCs, the chart, the CLI's client-side install glue | which namespaces exist, their tier labels, the identity map, the allow-listed buckets and SSM roots, the AWS region |
| shadow mode by default (`confirm: false`) and the safety rails in [safety.md](safety.md) | flipping `confirm: true`, and the sweep RBAC that goes with it |
| the tier-label reach mechanism | which label VALUES exist and what TTL each one gets |

## Shadow mode is not a feature flag to skip

Every deployment starts with `confirm: false` and stays there until a
human has read the sweep records the loop produced and decided the
plan is trustworthy. There is no supported install path that starts
`confirm: true`; the chart's default is the doctrine, not a suggestion.

## The identity resolver question is open, not decided

gemaal carries its own `pkg/identity` evidence-chain and email→slug
`Resolve` logic alongside access-roster's own resolver — the same
concern implemented twice, in two repositories, for historical reasons
(gemaal's predates the RPC that was meant to retire it). Whether to
converge on one implementation is an owner decision this repository has
not made; see the CHANGELOG and open issues rather than assuming either
side is the intended end state.
