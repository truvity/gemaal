# Adoption

## Prerequisites

- **Kubernetes:** a cluster with a tier-label convention already in use
  on the namespaces gemaal should watch (the key is
  `config.tierLabel`, required — see [safety.md](safety.md#tier-label-containment)), Helm 3
  with OCI registry support, and `kubectl`/`helm` reachable from the
  cluster the service runs in (the image ships both binaries next to
  `gemaal` for exactly this — see [reference.md](reference.md)).
- **AWS**, only if the SSM or S3 sweep is used (`allowList.ssmRoots` /
  `allowList.s3Buckets` non-empty): a region, and one of the two
  credential paths below.
- **access-issuer**, reachable from the cluster, if the console or a
  person's token is to be verified rather than trusted only via
  TokenReview (`authz.issuerURL`/`audience`).
- **Envoy Gateway**, if the web console is exposed to browsers: browser
  sign-in is gateway-owned (a `SecurityPolicy` with `oidc:`), not
  something this chart runs itself.

## Install order

1. **Read [config.example.yaml](../config.example.yaml)** and write the
   deployment's own values: the tier TTLs, the tier label (if not the
   default), the AWS region, the allow-listed buckets/roots, and the
   identity map rendered from the personnel source.
2. **Install with `confirm: false`** (the chart default) and the sweep
   RBAC left off (`rbac.sweep.enabled: false`, the default). This is
   shadow mode: the loop plans and reports, deletes nothing.
   ```bash
   helm install gemaal oci://ghcr.io/truvity/charts/gemaal --version 0.24.3 \
     -f values.yaml
   ```
3. **Read the sweep records** at `/sweeps` (or the structured log lines)
   for a full TTL cycle or two. This is the point of shadow mode — what
   *would* have been deleted is inspectable before anything is.
4. **Grant sweep RBAC and flip `confirm: true`** together, once the
   plan is trusted. They are meant to move together: sweep RBAC with
   `confirm` still false grants nothing usable, and `confirm: true`
   with no sweep RBAC fails at the first delete.
5. **Expose the console**, if wanted, through the gateway's own
   `SecurityPolicy` OIDC exposure pattern — `exposure.enabled`/
   `exposure.hostname` in the chart only document the hostname next to
   the app; the Gateway, HTTPRoute and OIDC client are the deployer's.

## AWS access

The service uses the AWS SDK's default credential chain — the chart
takes no side in how credentials arrive. Two equivalent setups:

- **EKS Pod Identity** (the common case): create a pod-identity
  association binding the role to the ServiceAccount's exact
  `(namespace, name)` pair, and pin `serviceAccount.name` so the pair
  holds. The agent injects only the credential endpoint — never
  `AWS_REGION`, which is why `config.awsRegion` is required with no
  fallback to the environment (see
  [reference.md](reference.md#pkgconfig-fields)).
- **IRSA**: set `serviceAccount.annotations` to
  `eks.amazonaws.com/role-arn: <role-arn>` and trust the cluster's OIDC
  provider account-side. On non-EKS clusters, the
  [truvity/amazon-eks-pod-identity-webhook](https://github.com/truvity/amazon-eks-pod-identity-webhook)
  fork provides the same projection.

The chart's NetworkPolicy admits both paths explicitly: STS rides the
general 443 egress (the whole IRSA credential path), and the link-local
pod-identity agent (`169.254.170.23:80`) is allowed for Pod Identity and
simply idle under IRSA.

## Defaults that are gemaal's own product surface

One chart value carries a default even though every other cluster-
specific value is required. It is not an estate fact this repository
guessed at — it is gemaal's own API surface, the way a library ships
a default port:

- `config.identity.personalNamespace` (`emp-{slug}`): the template a
  resolved slug's standing namespace renders from. Change it only if the
  deployment's namespace-naming convention differs.

`config.tierLabel` (for example `example.com/tier`) is required, with no
default: which namespace label an estate uses to tier its namespaces is
that estate's convention. The chart schema refuses an empty or malformed
key and the service refuses to start without one.

Every other cluster-specific value — the AWS region, the tier TTLs, the
allow-listed buckets and roots, the identity map — is required, with no
default. See [safety.md](safety.md#config-load-pkgconfig) for what an
omitted one refuses.

## Upgrading to a release with a required tier label

Earlier releases defaulted `config.tierLabel` to an organisation-specific
key. That default is gone. Before bumping the chart or image pin, set
`config.tierLabel` in the deployment's values to the label key the
cluster's namespaces already carry (the value the old default resolved to,
if the deployment never set one). Setting it first is harmless on the old
release, which already accepts the key, so the bump itself changes
nothing. A render without it fails schema validation.
