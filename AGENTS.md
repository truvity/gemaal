# AGENTS.md

Instructions for AI coding agents working with gemaal — in this repo or in a
repo that consumes it. Human-readable too; nothing here is agent-only.

## What this repo ships

| Binary | Is |
| --- | --- |
| `gemaal` | the service — ephemeral installation orchestration |
| `gemaalctl` | the human/CI face |

`gemaal` is the **repository and the service**. `gemaalctl` is the command you
type. Prose that says "gemaal installs it" means the system; a code block that
says `gemaal install` is wrong.

## The gemaalctl command surface, exhaustively

```
gemaalctl version
gemaalctl whoami                     # identity evidence chain + resolved tenant
gemaalctl install       [--chart …] [--infra-chart …] [--ttl …] [--keep-until …]
gemaalctl uninstall                  # ring pair: <release>, then <release>-infra
gemaalctl decommission               # explicit end-of-life, NOW
gemaalctl plan          --namespace … --release …
gemaalctl checkout      --namespace … --release … --for …
gemaalctl extend        --namespace … --release … --for …
gemaalctl pipeline snapshot
gemaalctl pipeline push-preview
gemaalctl pipeline release-stable
```

That is the whole of it. In particular:

- **There is no `gemaalctl up`.** It has never existed. If you are migrating a
  reference from the retired `barctl test --project=X --suffix=Y up`, the
  nearest real command is `gemaalctl install`, and it does **not** take
  `--project` or `--suffix` — the tenant is *resolved*, from the identity
  evidence chain, not passed in. That is the whole design.
- `install` is a client-side `helm upgrade --install`. The service never
  installs anything; that separation is the point.

Source of truth: [`cmd/gemaalctl/main.go`](cmd/gemaalctl/main.go) and
[`cmd/gemaalctl/tenant.go`](cmd/gemaalctl/tenant.go). If this file and those
disagree, they are right and this one is a bug — fix it here.

## If you are about to write a gemaalctl command into documentation

Read those two files first. Not the README, not a grep of how often a string
appears in other repos' prose, and not another document that might itself be
guessing.

This warning is not hypothetical. On 2026-08-16 an agent updating a consumer
repo's docs was told "test installs are gemaal's now" and turned that
repo-level fact into `gemaalctl up --project=${project} --suffix=${suffix}`,
carrying the flags over from the retired tool and inventing the verb. It then
"verified" by grepping documentation, which measures how often a mistake has
been repeated rather than whether it is true. `up` appears nowhere in this
repo.

**A plausible command that fails is worse than an admission that you do not
know the command.** If you cannot verify an invocation, write that it is
unverified and move on.

## Using gemaalctl from another repo

Pin it in `go.mod` with a `tool` directive, add a `bin/gemaalctl` wrapper that
`exec`s `go run`, and put `bin/` on PATH via devbox:

```go
tool (
	github.com/truvity/gemaal/cmd/gemaalctl
)
```

```bash
#!/usr/bin/env bash
set -euo pipefail
exec go run github.com/truvity/gemaal/cmd/gemaalctl "$@"
```

The wrapper carries no version, so it cannot disagree with the pin. Do not add
gemaalctl to `devbox.json` packages: nixpkgs would then own the version
alongside `go.mod`, and two owners is how a tool ends up one version on a
laptop and another in CI with nothing reporting the difference.

## Working in this repo

```bash
devbox shell
just check      # must pass before a PR
```
