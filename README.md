# gemaal

> **Early development.** This is the G1 scaffold: the design, the proto
> surface, and compiling skeletons. The API, configuration format, and
> everything else may change without notice until v0.1.0.

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

## Three faces

| Face | What it is |
|---|---|
| **`gemaal`** (service) | in-cluster watcher: TTL housekeeping over test tenants, ring-pair-aware teardown, orphaned-artifact sweeps, a ConnectRPC API (Plan / ListTenants / Checkout / Extend / Sweep / Resolve), and later a web console |
| **`gemaalctl`** (CLI) | tenant resolution and install labeling for humans and CI; ConnectRPC client for plan / checkout / extend |
| **Go library** | what test harnesses import: resolve the tenant, stamp the ledger labels, run |

## Status

| Phase | Contents | |
|---|---|---|
| G1 | scaffold: design doc, `proto/gemaal/v1`, service + CLI skeletons | ✅ |
| G2 | package move (kubewho, test harness, fleet config) + gemaalctl proper | ⏳ |
| G3 | service v1: housekeeping loop, real RPCs, auth, web console | ⏳ |
| G4 | v0.1.0 release and first deployment (shadow mode first) | ⏳ |

## Design

The tenancy model — tenant identity, tier labels, the
`gemaal.io/{ttl,keep-until,execution-id}` label ledger, uniform TTL, the
garbage contract, the driver families, and the safety rails — lives in
[docs/design.md](docs/design.md). Read that first; the code is its
shadow.

## Development

Toolchain via [devbox](https://www.jetify.com/devbox/) (+ direnv), tasks
via [just](https://just.systems/):

```bash
just check      # build + test + lint + vuln — what CI runs
just generate   # regenerate gen/ from proto/ (buf; output is committed)
just run        # run the service skeleton against config.example.yaml
```

## License

[MIT](LICENSE)
