# gemaal — the tenancy model

**Status:** initial design, extracted from the private operations
playbook it grew up in and made generic. Everything here is
deployment-agnostic: cluster names, bucket names, tier numbers and
allow-list values are *configuration*, not design.

## The problem

A team shares one test cluster. Installations happen constantly — an
engineer's standing install, a CI job's throwaway one — and each claims
names inside shared resources: a prefix in a shared bucket, a subtree in
a shared parameter store, a database in a shared cluster, tagged objects
in a sandbox organization. Installing is cheap and frequent by design;
what keeps the cluster habitable is not a gate in front of installs but
a pump behind them.

gemaal is that pump. It watches, ages, and drains what nobody is using
anymore.

## Watcher, not deployer

gemaal **never installs anything**. Clients run `helm upgrade --install`
(or `pulumi up`) themselves, stamping the ledger labels as they do. The
service's only verbs are *uninstall* and *sweep*.

This is the load-bearing decision:

- **A down gemaal delays cleanup; it never blocks anyone's loop.**
  Installation has no dependency on the service being up, current, or
  correct.
- The service needs no install-time credentials, no chart knowledge, no
  opinion about what a deployment contains.
- Failure is one-sided: the worst a broken gemaal can do is *not clean
  up*, or — bounded by the safety rails below — remove something inside
  the test estate that would have been reinstalled anyway.

## Tenant identity

**A tenant is one installation. Its identity is the tuple the tenancy
driver watches; every other name derives from it, nothing is
independently configurable, so nothing can disagree.**

| Driver family | Tenant identity |
|---|---|
| helm | `(namespace, release)` |
| pulumi | `(stack)` |

Typical shape (helm family): an engineer's standing install lives in a
personal namespace with the application's name as the release
(`emp-jdoe` / `myapp`); a CI install lives in a per-repo namespace with
a per-run release (`ci-myorg-myapp` / `r123-a1`).

Derived names, for a tenant `<ns>/<rel>`: an object-store prefix
`<ns>/<rel>/` in the shared test bucket, a parameter-store subtree
`<test-root>/<ns>/<rel>/...`, a database `t_<ns>_<rel>` (sanitized),
execution-tagged objects in the sandbox organization. The derivation
*asserts* platform name limits (Helm's 53-character release cap) rather
than discovering them in production.

**Execution id ≠ tenant id.** A tenant is an install — stable for an
engineer, per-run for CI. An *execution* is one test run inside it.
Everything a test creates carries the execution id, and assertions
filter by it — which is what makes re-running tests in a standing
install the normal case rather than an edge case.

### Tiers

Namespaces in scope carry a **tier label** (label key is deployer
configuration). The name prefix (`emp-`, `ci-`) is a human convention;
**the label is the machine selector** — RBAC, allow-lists and tier
defaults key on it, never on string prefixes. A namespace without the
tier label does not exist as far as gemaal is concerned.

Tiers differ **only in configuration** (TTL defaults, grace periods).
There is no structural difference between an "employee" tier and a "CI"
tier — see *Uniform TTL* below.

## The ledger: labels, no store

gemaal keeps **no database**. The ledger is labels on the watched
resources themselves — Helm release Secrets (helm ≥ 3.13 `--labels`),
pulumi stack tags:

| Label | Meaning |
|---|---|
| `gemaal.io/ttl` | this tenant's time-to-live; absent → tier default |
| `gemaal.io/keep-until` | absolute timestamp; overrides TTL until it passes |
| `gemaal.io/execution-id` | the test run that last touched this tenant |

The label domain defaults to `gemaal.io` and is configurable
(`labelDomain`) — a deployment may brand its own.

Because state lives on the resources, the loop is **level-triggered**:
every tick re-derives the world from what exists. Nothing survives a
restart because nothing needs to — there is no state but the cluster
(and, for the pulumi family, the stack backend).

## Uniform TTL

**One mechanism: TTL from last activity, plus keep-until.** There is no
"standing installs are never swept" special case; tiers differ only in
numbers (an employee tier might default to end-of-day, a CI tier to
24 hours — values live in deployer configuration).

Rationale: re-installation is cheap and safe *by construction* (the
re-run semantics above), and idle standing installs are the dominant
cost of a shared test cluster — a fleet of untouched per-install
databases overnight dwarfs everything CI leaves behind. The human
override is explicit and bounded: `Checkout`/`Extend` stamp
`keep-until`, and the tenant lives exactly that much longer.

## The garbage contract

> A **tenant** expires when its TTL (from last activity) has passed and
> no `keep-until` holds it. An **artifact** is garbage iff its tenant no
> longer exists and the artifact is older than a grace period.

Consequences, in order of importance:

- a tenant someone is actively using is never touched — activity resets
  the clock, and Extend can always buy time;
- an expired tenant dies automatically: teardown removes the release,
  its artifacts become orphans, the sweep collects them after grace;
- execution-tagged objects in the sandbox organization are garbage
  purely by age — executions are short-lived by definition;
- "clean before every run" is explicitly **not** the contract: tests
  tolerate pre-existing data (execution-id filtering), cleanup runs
  behind, and a failed cleanup never blocks testing.

### Teardown ordering: ring pairs

Installations commonly come as a pair of releases: the application
(`<rel>`) and its infrastructure dependencies (`<rel>-infra` —
databases, queues, buckets-as-claims). Teardown is **ring-pair aware**:
`<rel>` is uninstalled before `<rel>-infra`, so an application never
outlives the infrastructure it is connected to mid-drain.

## Driver families

### Tenancy drivers

A tenancy driver knows how to enumerate and dismantle one kind of
installation. The contract is three verbs, spoken over internal
ConnectRPC:

```
ListTenants()            → the tenants this driver sees, with ledger state
Describe(tenant)         → detail for one tenant
Teardown(tenant)         → dismantle it (ring-pair aware where applicable)
```

- **helm** — in-core: watches release Secrets in tier-labeled
  namespaces, uninstalls via the Helm SDK.
- **pulumi** — a separate workload with its own (sandbox-scoped)
  credentials: `pulumi destroy` via the automation API replays backend
  state, so no program source is needed. Isolated because its
  credential shape and blast radius differ from the in-cluster helm
  driver's.

The family is open: any system where an installation has an enumerable
identity and a destructor can become a driver.

### Identity drivers

Identity answers "which tenant is *mine*?" and splits deliberately in
two:

- **Client-side evidence drivers** extract an **email** from the
  environment, nothing more: `kubectl` (the authenticated username from
  `kubectl auth whoami`), `aws` (the SSO role session name), `env` (an
  explicit variable — always wins; what CI sets). The drivers chain
  with clear precedence: env, then kubectl, then aws; the first email
  wins, a driver with nothing to say is skipped with a recorded reason,
  and a *broken* driver (or a malformed explicit override) stops the
  chain instead of handing the answer to the next one.
- **The service is the resolution authority**: `Resolve(email) → slug,
  namespace`. Its mapping is rendered from the same personnel source
  that feeds cluster RBAC, so there is exactly one place where "who is
  this" is decided — clients present evidence, they never carry the
  mapping.

Until a deployment runs the service, the resolver interface is the seam
two stand-ins plug into, in override order:

1. **A committed email→slug map** (the `gemaal.yaml` identity section) —
   the same rendered mapping, checked in, for repos that accept carrying
   a copy of it.
2. **The kubectl-groups resolver — the sanctioned INTERIM for repos that
   commit no people data.** Stateless: it asks the cluster itself
   (`kubectl auth whoami -o json`) and reads the slug out of the
   caller's own prefixed token group (`emp:{slug}`; the prefix is
   configurable). The group claim is rendered from the same personnel
   source the service will read, so the answer is the authority's — just
   fetched through the caller's own token instead of an RPC. CI never
   reaches it: `GEMAAL_NAMESPACE` wins the namespace ladder outright.

The service's Resolve RPC stays the end state; when it ships, the RPC
client replaces these rungs behind the same interface — a staged
retirement, callers untouched.

## The client side

The library and CLI are the half of the contract that *creates* what
the service watches. Three rules keep the two halves honest:

**One resolution path.** Tests, `gemaalctl whoami` and
`gemaalctl install` resolve the tenant through the same ladders, so
their answers cannot drift:

```
namespace: explicit option → GEMAAL_NAMESPACE →
           identity chain → email → slug → personal-namespace template
release:   explicit option → GEMAAL_RELEASE →
           CI (r{run}-a{attempt}) → the application name
```

Both halves are bounds-asserted at resolution time (63 characters and
RFC1123 shape for the namespace, Helm's 53 for the release — including
the derived `<release>-infra` companion). The resolved pair is
re-exported as `GEMAAL_NAMESPACE` / `GEMAAL_RELEASE` /
`GEMAAL_KUBECONTEXT` — the variables are bidirectional: read as
overrides, always written back with the resolved values.

**Standing tenants only.** The harness itself creates and deletes
NOTHING — no namespace lifecycle. A test run resolves its standing
tenant, runs inside it, and leaves; re-runs are the normal case
(execution-id filtering, tolerant assertions), and draining what nobody
uses anymore is the service's job, not the test's. The predecessor's
ephemeral create-run-destroy mode was deliberately not carried over.
(A suite MAY declare its own teardown hooks over its own releases — the
sanctioned interim until a deployment runs the service's TTL
housekeeping, at which point leaving the pair standing becomes the
default and the hooks retire.)

**A suite is phases around `m.Run()`.** Real integration suites do not
just resolve and run: they build artifacts, install them, wait for
rollouts and resolve service URLs first — and tear their releases down
after. The library owns that bracket (`harness.Run` over a `Suite`:
`Build` → `Deploy` → `Setup` → tests → `Teardown`) together with the
operator's `GEMAAL_TEST_*` skip contract: `SKIP_BUILD` reuses the
artifacts lying around, `SKIP_DEPLOY` reuses the standing releases as
installed (and implies `SKIP_BUILD`), `SKIP_DESTROY`/`KEEP` keep the
releases afterwards. `Setup` is never skipped — a reused install still
has to be ready. The flags parse strictly: a set-but-unparseable value
refuses the run rather than, say, misreading "keep" and destroying what
it was told to preserve. Teardown registers before anything installs
(partial installs are torn down too), runs under its own context (a
canceled run still cleans up), and its failures are reported without
overriding the test verdict — what a failed teardown leaves behind is a
standing tenant, which is exactly what the pump exists to drain.

**Installs stamp the ledger.** The install helpers run
`helm upgrade --install` client-side (helm ≥ 3.13, for `--labels` on
the release Secret), stamping `ttl` / `keep-until` / `execution-id`
under the configured label domain, installing ring pairs in order
(`<release>-infra` first) and uninstalling in reverse. Per-cluster
values from the committed `gemaal.yaml` ride along as `.Values.gemaal`,
opaque to the tooling — organization semantics live in the charts.

Charts reading `.Values.gemaal` is a **consumer-side migration**, not
something the tooling can force: a chart whose values contract predates
gemaal keeps it, and the suite maps the cluster values onto that
contract by hand (bar does exactly this today — friction #3 of the
adoption feedback, accepted for now). The client-side tier helper
(`TierForNamespace`: `emp-` → employee, `ci-` → ci, else unknown)
exists for the same reason — it is the one rule such mapping code kept
re-implementing. It is a client convenience only: on the service side
the tier LABEL stays the machine selector, never the name prefix.

## Safety rails

The janitor half of gemaal deletes things for a living, so its reach is
bounded structurally, not by care:

1. **Allow-listed roots, not patterns.** The configuration names the
   only places sweeps may touch: tier-labeled namespaces, specific
   buckets, specific parameter-store roots. Nothing outside the list is
   reachable by any code path — there is no regex whose false positive
   could wander into production.
2. **`/secrets/**` is refused**, structurally and unconditionally. The
   secrets tree has a different writer and a different lifecycle; it is
   not gemaal's to manage, ever.
3. **Non-test-shaped configuration refuses to load.** Parameter-store
   roots must live under the test root (`/test/` by convention). A
   configuration that points the sweeper at something that does not
   look like a test estate is a deployment error, and the service says
   so instead of starting.
4. **Dry-run is the default.** A fresh deployment plans and reports;
   executing deletions is a deliberate configuration flip. (The
   expected rollout is shadow mode first: run Plan alongside whatever
   cleanup exists today, compare, then enable.)
5. **Structured deletion records.** Every destructive action emits a
   record — what, why (which rule, which numbers), when, by which
   execution of the loop — so the history of the pump is auditable
   after the fact.

## API surface

`proto/gemaal/v1` — one ConnectRPC service, six RPCs:

| RPC | Face | What it does |
|---|---|---|
| `Plan` | janitor | compute, without side effects, what housekeeping would do now (each action carries the rule that fired and a human-readable reason) |
| `ListTenants` | console | enumerate watched tenants with ledger state and pending actions |
| `Checkout` | tenant lifetime | claim a tenant slot for the caller, stamping `keep-until` |
| `Extend` | tenant lifetime | push a tenant's `keep-until` forward |
| `Sweep` | janitor | execute the current plan (subject to the server's dry-run setting) |
| `Resolve` | identity | map an email to its slug and namespace |

Mutating RPCs are authenticated by the deployment (in-cluster: token
review against the caller's service account; humans through a gateway
that forwards verified identity). The API is small on purpose: install
is not here, because installing is not gemaal's job.
