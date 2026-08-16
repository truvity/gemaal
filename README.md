# gemaal

> **Early development.** The design, the proto surface, the client
> library, the CLI and the service are in place; the service has not
> yet seen a production deployment (that is G4, shadow mode first).
> The client API is in real use (bar's url-shortener suite) but may
> still change between 0.x minors without a compatibility shim.

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

## Three faces

| Face | What it is |
|---|---|
| **`gemaal`** (service) | in-cluster watcher: TTL housekeeping over test tenants, ring-pair-aware teardown, orphaned-artifact sweeps, the six ConnectRPC RPCs (Plan / ListTenants / Checkout / Extend / Sweep / Resolve), and the web console |
| **`gemaalctl`** (CLI) | `whoami` (the identity evidence chain + the resolved tenant), `install`/`uninstall` (client-side helm with the ledger labels stamped, ring-pair aware), ConnectRPC client for plan / checkout / extend, and the artifact pipeline |
| **Go library** | what test harnesses import: `pkg/harness` (resolve the standing tenant, bracket the suite's build/deploy/setup/teardown phases, install helpers), `pkg/identity` (evidence drivers + slug resolution, incl. the interim kubectl-groups resolver), `pkg/gemaalcfg` (the committed `gemaal.yaml`) |

## Status

| Phase | Contents | |
|---|---|---|
| G1 | scaffold: design doc, `proto/gemaal/v1`, service + CLI skeletons | ✅ |
| G2 | identity drivers + standing-tenant harness + `gemaal.yaml` + gemaalctl proper | ✅ |
| G3 | service v1: housekeeping loop, real RPCs, auth, web console, helm chart | ✅ |
| G4 | service image + chart publishing, first deployment (shadow mode first) | ⏳ |

## Documentation

| Section | Contents |
| --- | --- |
| **Usage** | [The test harness](docs/usage/harness.md) — resolve a standing tenant, bracket a suite, the install helpers |
| **Reference** | [gemaalctl commands](docs/reference/cli.md) · the `GEMAAL_TEST_*` contract · resolution ladders |
| **Architecture** | [The service — the pump](docs/architecture/service.md) — reach, release truth, the rules, auth, the console |
| **Operations** | [Running the service](docs/operations/running-the-service.md) — shadow mode, RBAC split, the exec boundary, known stubs |
| **Design** | [The tenancy model](docs/design.md) — tenant identity, the label ledger, uniform TTL, the garbage contract |
| **Development** | [Contributing](docs/development/contributing.md) · [Changelog](CHANGELOG.md) |
| **Agents** | [AGENTS.md](AGENTS.md) — the exhaustive command surface and the rules for documenting it |

## Design

The tenancy model — tenant identity, tier labels, the
`gemaal.io/{ttl,keep-until,execution-id}` label ledger, uniform TTL, the
garbage contract, the driver families, and the safety rails — lives in
[docs/design.md](docs/design.md). Read that first; the code is its shadow.


## License

MIT — see [LICENSE](LICENSE).
