# gemaal

> **Early development.** The design, the proto surface, the client
> library and the CLI are in place; the service is still a skeleton. The
> API, configuration format, and everything else may change without
> notice until v0.1.0.

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
| **`gemaalctl`** (CLI) | `whoami` (the identity evidence chain + the resolved tenant), `install`/`uninstall` (client-side helm with the ledger labels stamped, ring-pair aware), ConnectRPC client for plan / checkout / extend, and the artifact pipeline |
| **Go library** | what test harnesses import: `pkg/harness` (resolve the standing tenant, run, install helpers), `pkg/identity` (evidence drivers + slug resolution), `pkg/gemaalcfg` (the committed `gemaal.yaml`) |

## Status

| Phase | Contents | |
|---|---|---|
| G1 | scaffold: design doc, `proto/gemaal/v1`, service + CLI skeletons | ✅ |
| G2 | identity drivers + standing-tenant harness + `gemaal.yaml` + gemaalctl proper | ✅ |
| G3 | service v1: housekeeping loop, real RPCs, auth, web console | ⏳ |
| G4 | v0.1.0 release and first deployment (shadow mode first) | ⏳ |

## The test harness

A project's integration tests resolve their standing tenant once, in
TestMain, and run inside it — the harness creates and deletes nothing
(cleanup is the service's job, driven by the labels installs stamp):

```go
func TestMain(m *testing.M) {
    cfg, err := gemaalcfg.Load("gemaal.yaml")
    if err != nil {
        log.Fatal(err)
    }

    os.Exit(harness.Run(m, harness.Options{
        Kubecontext: "devel@oidc",
        App:         "url-shortener",
        Config:      cfg,
    }))
}
```

Resolution ladders (each rung explicit, first hit wins):

- **namespace**: `Options.Namespace` → `GEMAAL_NAMESPACE` → identity
  chain (`GEMAAL_EMAIL` → `kubectl auth whoami` → AWS SSO session) →
  email → slug → the `personalNamespace` template (`emp-{slug}`)
- **release**: `Options.Release` → `GEMAAL_RELEASE` →
  CI (`r{run}-a{attempt}`) → `Options.App`

The resolved pair is exported as `GEMAAL_NAMESPACE` / `GEMAAL_RELEASE` /
`GEMAAL_KUBECONTEXT`. Installs go through `harness.Cluster`: helm ≥ 3.13
`--labels` stamping (`gemaal.io/{ttl,keep-until,execution-id}`),
ring-pair ordering (`<rel>-infra` installs first, uninstalls last),
rollout waits and Service ClusterIP resolution. `gemaal.example.yaml`
documents the committed per-repo configuration.

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
