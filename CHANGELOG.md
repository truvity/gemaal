# Changelog

One heading per release; full detail lives in the [release
notes](https://github.com/truvity/gemaal/releases) and the git history.

## v0.25.1

- **Breaking:** `config.tierLabel` is required and no longer defaults to an
  organisation-specific label. The chart schema refuses an empty or malformed
  key, `pkg/config` drops `DefaultTierLabel`, and the service refuses to start
  without one. Set it explicitly before upgrading; see
  [docs/adoption.md](docs/adoption.md#upgrading-to-a-release-with-a-required-tier-label).

## v0.25.0

- **Breaking for callers of `pkg/config`:** the AWS region is required and no longer defaults to `eu-central-1`.
- Leak hygiene: real account IDs and private repository names removed from examples and comments; `hack/leak-canary.sh` runs in `just check`.
- README rewritten in the component contract's heading order with `Consumers` and `Neighbours`; stale claims about image and chart publishing and the console's proxy removed; `docs/` gains adoption, doctrine, reference and safety pages; CHANGELOG per tag.
- `github.com/truvity/access-roster` bumped to v1.39.1.

## v0.24.3
- `(*Cluster).WaitForDeploymentsAtVersion`: `WaitForDeployments`
  (`kubectl rollout status`) proves a Deployment finished rolling out, but
  not WHICH generation — called before a controller has pushed the new
  spec at all, it sees the OLD generation already fully rolled out and
  returns immediately. A test-chart Job a GitOps controller applies while
  the app release's own rollout is still catching up hits exactly this: the
  e2e suite's readiness wait passed against the PREVIOUS version's pods.
  The new call polls each Deployment's own JSON — the same fields
  `kubectl rollout status` reads — until its pod template carries a given
  `versionLabel=want` AND that generation is fully, availably rolled out;
  a stuck rollout's error names the Deployment and the one condition still
  unmet.

## v0.24.2
- Dependency bumps only: `truvity/ci-workflows` to v3.12.2 (via v3.7.1,
  v3.9.0) and non-major Go modules.

## v0.24.1
- `(*Cluster).ReleaseDeployments` (and `WaitForDeployments`, which is
  built on it) now also recognizes a Deployment by its
  `app.kubernetes.io/instance` label, falling back to it whenever the
  `meta.helm.sh/release-name` annotation is absent. A release a GitOps
  controller renders with `helm template` and applies directly — never
  running `helm install/upgrade` — carries the standard chart label but
  none of helm's own release annotations, so the harness previously
  found no Deployments to wait on. The annotation still wins when both
  are present.

## v0.24.0
- The harness gains a kind tier, for public repos that run the same e2e
  suites against a disposable [kind](https://kind.sigs.k8s.io/) cluster
  instead of the shared one: `harness.DetectTier` (`GEMAAL_TIER=kind`,
  or a `kind-*` kube context) is a different axis from
  `TierForNamespace`'s namespace-name convention, and it is what
  `(*Cluster).ServiceURL` now checks — on kind, where no service-CIDR
  route exists, it opens a `kubectl port-forward` straight to the
  Service's Pod and returns a local URL instead of dialing the
  ClusterIP directly, with the same call site as every other tier.
  `harness.DeployApp` installs ring3 alone, mirroring `DeployInfra`'s
  ring2-alone counterpart, for kind's fixture-replaces-ring2 lane.

## v0.23.1
- The ko image name follows KO'S precedence, which is
  `preserve_import_paths`, then `base_import_paths`, then `bare` — not
  the order the fields appear in. Every config in this estate sets
  `bare: true` AND `base_import_paths: true` on the same entry, and ko
  resolves that to `base_import_paths`. 0.23.0 read `bare` first and
  named the image after the repository's PARENT, which a registry
  answers with 403 Forbidden when the release role may push to one
  child path and not to the parent. Exactly one naming flag is passed
  now, so the name computed here is not a guess about which one ko
  would pick.

## v0.23.0
- goreleaser-pro leaves the pipeline. The dev loop ran `goreleaser
  release --nightly`, which is Pro-only and bought two things: a build
  from a dirty tree, now `--skip=validate` on the OSS build, and a ko
  publish. `--snapshot` implies `--skip=publish`, so goreleaser builds
  the ko images and pushes nothing — `publishKoImages` is the push, in
  the same shape `publishImages` already gives the `dockers_v2` images,
  and `commands.ko` is the new tool (default `ko`). A consumer must
  therefore carry `ko` in its toolchain, and may drop goreleaser-pro and
  its licence.

## v0.22.8
- The web console moves off the (retired) access-proxy pattern onto
  Envoy Gateway's own OIDC filter: sign-out now targets the gateway's
  `/oauth2/logout` (the client row's proxy prefix plus `/logout`)
  instead of oauth2-proxy's `/oauth2/sign_out`, and sign-in works by
  loading the console itself — the gateway's OIDC filter gates every
  request, so there is no `/oauth2/start`-shaped endpoint to call;
  loading `/` is what triggers the redirect to the issuer when there is
  no session.
- Dependency bumps: `github.com/truvity/access-roster` to v1.17.0 (via
  v1.16.0, v1.16.3) and devbox packages.

## v0.22.7
- Dependency bump only: `github.com/truvity/access-roster` to v1.15.0.

## v0.22.6
- Pins every reusable-workflow call at `truvity/ci-workflows` v3.0.1;
  v3.0.0 removed three reusable workflows this repository does not call,
  so the bump is a pin change only. Plus non-major dependency bumps,
  including `access-roster` to v1.10.0.

## v0.22.5
- Dependency renovation and toolchain parity move to the organisation's
  shared `ci-caller` repository, which now runs them for every enrolled
  repository from one place; this repository's own renovate config and
  `devbox.json` are unchanged, only where the jobs run. Plus a
  dependency bump: `access-roster` to v1.8.0.

## v0.22.4
- `authz.userinfoURL` is removed from the config schema. v0.22.3
  accepted and ignored it for one release so deployed values kept
  starting while they moved to `authz.issuerURL`/`audience`; once every
  deployment had moved, the strict config went back to refusing it like
  any unknown key.

## v0.22.3
- People's tokens are now verified against access-issuer's signing keys
  and audience, using access-roster's own identity package, instead of
  being read off the gateway-forwarded JWT unchecked. TokenReview still
  answers first for workload callers; a token neither authority vouches
  for is refused, with no unverified fallback. `authz.issuerURL` and
  `authz.audience` configure the issuer — without an issuer only
  TokenReview callers authenticate, and an issuer without an audience
  refuses to start. The Zitadel-era userinfo enrichment goes; the
  console renders access-roster's own `UserBadge` component.

## v0.22.2
- Two fixes to the pipeline's digest-pinning, back to back: reading the
  post-push manifest digest as JSON instead of a `--format` template
  (the bare template stopped working the moment a build carried SBOM or
  provenance attestations, since the tag then resolves to an OCI image
  INDEX rather than a single manifest); then, as a more robust
  follow-up, hashing the raw manifest bytes `imagetools inspect --raw`
  returns instead of trusting any of buildx's own digest reporting —
  one path for a single manifest and an attestation-carrying index
  alike, verifiable against the registry.

## v0.22.1
- The harness can install ring 2 (the infra chart) alone, for a suite
  that needs a real database and none of the daemons — `InfraChartTgz`
  and `DeployInfra` sit beside `ChartTgzs`/`DeployPair` rather than a
  flag on them, since a missing app chart is an error for the pair but
  the normal state for the infra-only lane.
- The pipeline's provenance gate now records CI's own provenance
  (repository, run, attempt, runner, ref) when it detects it is running
  on GitHub Actions, instead of printing the laptop break-glass warning
  unconditionally on every stable release.

## v0.22.0
- `dockers_v2` images are pushed by digest and tagged once: one `docker
  buildx build --push` per platform, exported by digest (never a tag
  write), then one `imagetools create` per image writes the manifest
  list under the tags as the only, last, tag write. This is what lets
  the pipeline run against immutable registries with multi-node
  (multi-architecture) builders, where the old one-buildx-build-per-image
  shape wrote the same tag twice and the second write was refused.
- Build-context symlinks are copied as symlinks rather than followed —
  following them broke the first multi-node build whose context held a
  node_modules symlink a plain file copy cannot open, and following
  would have duplicated megabytes of a workspace's own linked packages
  besides.

## Earlier releases

v0.0.1 through v0.21.0 (41 releases) covered: the G1 scaffold (proto
surface, service and CLI skeletons, the tenancy design doc); G2
(identity evidence drivers, the `gemaal.yaml` config, the standing-tenant
harness, `gemaalctl whoami`/`install`/`uninstall`); the harness's
`Suite`-phased `Run(m)` and `GEMAAL_TEST_*` skip contract; G3 (the
housekeeping engine, the six RPCs, TokenReview auth, the web console, the
`charts/gemaal` chart, shadow mode by default); the SSM and S3 sweeps
going live behind their allow-list and test-shape tripwires; the
artifact pipeline's snapshot/push-preview/release-stable flows, ported
from an internal consumer repo's release scripts; and G4's first image
and chart publishing. This section is a summary, not a per-tag record —
see the [release
notes](https://github.com/truvity/gemaal/releases?q=&expanded=true) for
any tag in this range.
