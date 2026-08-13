package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// stableState tracks what a stable release has published — the port of
// release-stable.sh's trap bookkeeping. Three states per artifact, not
// two: goreleaser pushes many artifacts and can fail AFTER some of them
// landed, and helmctl can be interrupted after an upload landed remotely
// — so `started` flips BEFORE each invocation and `confirmed` only after
// it returns success. Started-but-not-confirmed means an UNKNOWN outcome,
// and the report says exactly that instead of pretending nothing landed.
type stableState struct {
	tag          string
	version      string
	chartVersion string

	goreleaserStarted bool
	publishedImages   bool
	// chartsSelected records that a coherent tarball pair from THIS run
	// exists: manual push advice naming the staged tarballs is only
	// honest after that point.
	chartsSelected  bool
	startedRing2    bool
	confirmedRing2  bool
	startedRing3    bool
	confirmedRing3  bool
	releaseComplete bool

	// stageDir is the private snapshot the pushes run from. Owned by the
	// partial-publish report: kept after a failure ONLY while its
	// tarballs are what the recovery advice tells the operator to push;
	// removed on every other exit.
	stageDir string
	infraTgz string
	appTgz   string
}

// ReleaseStable is the STABLE RELEASE (the port of release-stable.sh).
// One fully-gated flow: verify ALL five boundary conditions, then build
// AND push — images via goreleaser (full validation, no --nightly) and
// both ring charts via helmctl — in a single run. Nothing is
// configurable at run time: a stable release is BY DEFINITION the
// content of a <tagPrefix>v* tag on the pushed tip of the release
// branch, published to the stable registry.
//
// The five gates run BEFORE any build step — the preBuild commands
// included — and before any credential use:
//
//	a. clean working tree (untracked included);
//	b. HEAD equals origin/<releaseBranch> (equal — not merely an ancestor);
//	c. HEAD carries a <tagPrefix>v* tag;
//	d. that tag is pushed and the remote tag matches the local one;
//	e. CI provenance: the laptop path is the INTERIM stand-in for a CI
//	   release job — loud warning. There is deliberately NO escape hatch
//	   for gates a–d: gates that can be skipped aren't gates.
//
// NOT ATOMIC — and it deliberately does not claim to be. An OCI registry
// has no transaction: images, ring2 and ring3 are separate pushes, and a
// failure between them leaves the earlier ones published. What this flow
// does instead: publishes in strictly increasing order of consumability
// (images → ring2 → ring3, each less consumable than the next, so the
// most misleading one lands last — ring3 is the commit point: until it
// exists the version is not released, and every earlier artifact is
// harmless on its own), reports what did land — and what MIGHT have
// landed — when a step fails, and names the failure-point-specific
// recovery, truthful about the IMMUTABLE_WITH_EXCLUSION stable
// repositories where a pushed version tag can never be overwritten.
func (p *Pipeline) ReleaseStable(ctx context.Context) (err error) {
	if err := p.resolveRoot(ctx); err != nil {
		return err
	}

	// Stable release == stable ring, always.
	dest := p.cfg.Registries.Stable
	env := p.destinationEnv(dest, true)

	// Gates. All five run before goreleaser, before helmctl, before any
	// AWS credential use — a stable release either starts from a provably
	// published state or does not start at all.
	tag, err := p.stableGates(ctx)
	if err != nil {
		return err
	}

	p.log.Info("stable release", slog.String("tag", tag), slog.String("registry", dest.Registry))

	st := &stableState{tag: tag}
	defer func() { p.reportPartialPublish(st, err) }()

	// WRITER lock: held from the cleanup below through packaging, the
	// stamp write and the pre-push stage + validation — one acquisition
	// for the whole mutation window, goreleaser included. Dropped before
	// the registry login.
	release, err := p.acquireChartLock(ctx)
	if err != nil {
		return err
	}
	defer release()

	// 0. Invalidate the previous run's output BEFORE the first build
	// step — charts and stamp dropped together, rewritten only by a run
	// that reaches the stamp write.
	if err := p.cleanChartsOutput(); err != nil {
		return err
	}

	// 1. Pre-build commands (e.g. frontend bundles). Deliberately NOT the
	// caller's task deps: task runners execute deps BEFORE the command,
	// which would compile artifacts before the gates above ever rejected
	// a dirty or untagged tree — gates that run after a build are not
	// gates. They run here, after the gates pass.
	for _, argv := range p.cfg.Commands.PreBuild {
		if err := p.runner.Run(ctx, Command{Argv: argv, Dir: p.root, Env: env}); err != nil {
			return err
		}
	}

	// 2. Images. Full goreleaser validation (no --nightly, no --skip): it
	// re-verifies the clean tree and the exact tag, then publishes to the
	// stable account. The started flag flips BEFORE the invocation:
	// goreleaser can push a subset of the images and then fail, and the
	// report must not claim "nothing landed" in exactly the window where
	// partial publication is most likely.
	st.goreleaserStarted = true

	if err := p.run(ctx, env, p.cfg.Commands.Goreleaser,
		"release", "--clean", "-f", p.cfg.GoreleaserConfig); err != nil {
		return err
	}

	st.publishedImages = true

	// 3. Release manifest (version + digest-pinned image values).
	if err := p.run(ctx, env, p.cfg.Commands.Helmctl,
		"goreleaser-manifest", "--goreleaser-dist", p.cfg.DistDir, "-o", p.cfg.ManifestPath()); err != nil {
		return err
	}

	version, err := p.manifestVersion()
	if err != nil {
		return err
	}

	st.version = version

	// 4. Both ring charts (the writer lock from step 0 is STILL HELD —
	// goreleaser --clean cannot touch its inode, so no re-acquisition
	// happens anywhere in this flow).
	if err := p.packageCharts(ctx, env, version); err != nil {
		return err
	}

	// 5. Provenance stamp. The preview push refuses stable-stamped
	// charts: a stable chart set is pushed HERE, in the same run as its
	// build, or not at all.
	if err := p.writeStamp(StampStable); err != nil {
		return err
	}

	// Push. First FREEZE the build output: the charts directory is shared
	// with the dev loop, so the whole directory (tarballs + stamp) is
	// copied BLINDLY into a private temp dir, and everything after —
	// selection, the version cross-check, the pushes themselves — runs
	// against that immutable snapshot. No cleanup defer here: the
	// partial-publish report owns the stage, because after a failed push
	// the staged tarballs ARE the recovery artifacts its advice names.
	stage, err := p.stageCharts()
	if err != nil {
		return err
	}

	st.stageDir = stage

	sel, err := p.selectRingCharts(stage, p.cfg.Hints.Stable, p.cfg.ChartsOut())
	if err != nil {
		return err
	}

	st.chartVersion = sel.Version
	st.infraTgz = sel.InfraTgz
	st.appTgz = sel.AppTgz

	// Belt and braces: the packaged charts must carry the version this
	// run's manifest produced, not a version that survived from somewhere
	// else.
	if sel.Version != version {
		p.errf("error: packaged charts are version '%s', but this release built '%s'.\n", sel.Version, version)
		p.errf("       %s is holding charts this run did not produce.\n", p.cfg.ChartsOut())

		return errors.New("packaged charts do not carry this release's version")
	}

	// A coherent pair from THIS run exists — only from here on may the
	// report honestly tell the operator to push the staged tarballs by
	// hand.
	st.chartsSelected = true

	// The whole mutation window ran under ONE lock acquisition; the
	// shared dir no longer matters to this run. Drop the lock BEFORE the
	// login: the pushes read only the stage, and holding it through
	// registry uploads would stall a concurrent build for no protection
	// at all.
	release()

	if err := p.ecrLogin(ctx, env, dest); err != nil {
		return err
	}

	// Least-consumable artifact first: ring2 is inert on its own, ring3
	// is the commit point that makes the version look released, so ring3
	// goes last. Each push carries the same started/confirmed split as
	// goreleaser.
	st.startedRing2 = true

	if err := p.pushChart(ctx, env, dest, p.cfg.Charts.Ring2.Name, sel.InfraTgz, sel.Version); err != nil {
		return err
	}

	st.confirmedRing2 = true
	st.startedRing3 = true

	if err := p.pushChart(ctx, env, dest, p.cfg.Charts.Ring3.Name, sel.AppTgz, sel.Version); err != nil {
		return err
	}

	st.confirmedRing3 = true
	st.releaseComplete = true

	p.log.Info("stable release complete: images + both ring charts published",
		slog.String("tag", tag),
		slog.String("version", version),
		slog.String("registry", dest.Registry))

	return nil
}

// stableGates verifies the five boundary conditions and returns the
// release tag. Failures explain themselves on stderr, exactly like the
// spec scripts.
func (p *Pipeline) stableGates(ctx context.Context) (string, error) {
	branch := p.cfg.ReleaseBranch

	// Gate a: clean working tree, untracked files included.
	status, err := p.output(ctx, nil, []string{"git"}, "status", "--porcelain")
	if err != nil {
		return "", err
	}

	if strings.TrimSpace(status) != "" {
		p.errf("error: working tree is not clean (uncommitted changes or untracked files present).\n")
		p.errf("       A stable release builds EXACTLY what the release tag points at — nothing else\n")
		p.errf("       may leak into the images. Commit, stash, or remove the changes and re-run.\n")
		p.errf("hint: for iterating on uncommitted work use the dev loop:\n")
		p.errf("      %s\n", p.cfg.Hints.Snapshot)

		return "", errors.New("working tree is not clean")
	}

	// Gate b: HEAD must BE origin/<branch> — equal, not merely an
	// ancestor. Fetch first so the comparison is against the remote's
	// current tip. Side effect: if a local release tag diverged from
	// origin's, this fetch itself fails ("would clobber existing tag") —
	// a hard stop before any build; gate d's local-vs-remote comparison
	// is the defense-in-depth behind it.
	if err := p.run(ctx, nil, []string{"git"}, "fetch", "origin", branch, "--tags"); err != nil {
		return "", err
	}

	head, err := p.output(ctx, nil, []string{"git"}, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}

	remoteTip, err := p.output(ctx, nil, []string{"git"}, "rev-parse", "origin/"+branch)
	if err != nil {
		return "", err
	}

	head = strings.TrimSpace(head)
	remoteTip = strings.TrimSpace(remoteTip)

	if head != remoteTip {
		p.errf("error: HEAD is not origin/%s (after fetch).\n", branch)
		p.errf("       HEAD:          %s\n", head)
		p.errf("       origin/%s: %s\n", branch, remoteTip)
		p.errf("       Stable releases are cut only from the pushed tip of %s — not from\n", branch)
		p.errf("       branches, not from older %s commits. Merge + push first, then\n", branch)
		p.errf("       check out the up-to-date %s and re-run.\n", branch)

		return "", fmt.Errorf("HEAD is not origin/%s", branch)
	}

	// Gate c: HEAD must carry the release tag (goreleaser's monorepo
	// tag prefix).
	pattern := p.cfg.TagPrefix + "v*"

	tag, err := p.output(ctx, nil, []string{"git"},
		"describe", "--exact-match", "--match", pattern, "--tags", "HEAD")
	if err != nil {
		p.errf("error: HEAD carries no '%s' tag.\n", pattern)
		p.errf("       A stable release is the content of a release tag. Create and push one first:\n")
		p.errf("       git tag -a %svX.Y.Z -m '%s vX.Y.Z' && git push origin %svX.Y.Z\n",
			p.cfg.TagPrefix, p.cfg.Project, p.cfg.TagPrefix)

		return "", fmt.Errorf("HEAD carries no %s tag", pattern)
	}

	tag = strings.TrimSpace(tag)

	// Gate d: the tag must be pushed, and the remote tag must be the SAME
	// object as the local one (a local-only or diverged tag is not a
	// release).
	lsOut, err := p.output(ctx, nil, []string{"git"}, "ls-remote", "origin", "refs/tags/"+tag)
	if err != nil {
		return "", err
	}

	var remoteTagSha string
	if fields := strings.Fields(lsOut); len(fields) > 0 {
		remoteTagSha = fields[0]
	}

	if remoteTagSha == "" {
		p.errf("error: tag '%s' exists locally but was never pushed to origin.\n", tag)
		p.errf("       An unpushed tag is not a release — nobody else can reproduce it.\n")
		p.errf("hint: git push origin '%s'\n", tag)

		return "", fmt.Errorf("tag %s was never pushed to origin", tag)
	}

	localTagSha, err := p.output(ctx, nil, []string{"git"}, "rev-parse", "refs/tags/"+tag)
	if err != nil {
		return "", err
	}

	localTagSha = strings.TrimSpace(localTagSha)

	if remoteTagSha != localTagSha {
		p.errf("error: tag '%s' differs between local and origin.\n", tag)
		p.errf("       local:  %s\n", localTagSha)
		p.errf("       origin: %s\n", remoteTagSha)
		p.errf("       Someone re-pointed the tag. Resolve which one is real before releasing\n")
		p.errf("       (a moved release tag is an incident, not an inconvenience).\n")

		return "", fmt.Errorf("tag %s differs between local and origin", tag)
	}

	// Gate e: CI provenance. The laptop entrypoint IS the release path
	// until a CI release job exists — but only until then.
	p.errf("============================================================================\n")
	p.errf("  WARNING: stable release from a laptop\n")
	p.errf("  --------------------------------------------------------------------------\n")
	p.errf("  This command is the INTERIM stand-in for a CI release job. The moment CI\n")
	p.errf("  exists, this local path should be retired to break-glass only: stable\n")
	p.errf("  artifacts should have CI provenance, not laptop provenance.\n")
	p.errf("  Gates verified: clean tree, HEAD == origin/%s, tag on HEAD, tag\n", branch)
	p.errf("  pushed. What CANNOT be verified here: that this machine's toolchain\n")
	p.errf("  matches CI's. Proceeding.\n")
	p.errf("============================================================================\n")

	return tag, nil
}

// reportPartialPublish is the port of release-stable.sh's EXIT trap.
// Everything after the gates can publish, and nothing can be rolled
// back. The one thing that must never happen is a failed release that
// leaves an operator guessing what is out there — so say what landed,
// what MIGHT have landed, and the failure-point-specific recovery.
func (p *Pipeline) reportPartialPublish(st *stableState, failure error) {
	dest := p.cfg.Registries.Stable
	ring2 := p.cfg.Charts.Ring2.Name
	ring3 := p.cfg.Charts.Ring3.Name

	// Staged-snapshot cleanup. The snapshot is kept ONLY when the
	// recovery advice below is about to name its tarballs as the
	// manual-push input — images published, coherent pair selected, ring3
	// unconfirmed, release incomplete. Deleting it then would delete the
	// operator's recovery artifacts; every other exit removes it.
	if st.stageDir != "" {
		if failure == nil || st.releaseComplete ||
			!st.publishedImages || !st.chartsSelected || st.confirmedRing3 {
			_ = os.RemoveAll(st.stageDir)
		}
	}

	if st.releaseComplete || failure == nil {
		return
	}

	if !st.goreleaserStarted {
		// Failed before anything could push — nothing is out there,
		// nothing to say. (Chart pushes only happen after goreleaser, so
		// this flag gates every publish path.)
		return
	}

	versionOrTag := st.version
	if versionOrTag == "" {
		versionOrTag = st.tag
	}

	p.errf("\n")
	p.errf("============================================================================\n")
	p.errf("  PARTIAL RELEASE — %s did NOT complete\n", st.tag)
	p.errf("  --------------------------------------------------------------------------\n")

	if !st.publishedImages {
		p.errf("  POSSIBLY PARTIALLY PUBLISHED to %s:\n", dest.Registry)
		p.errf("    * container images for %s — goreleaser was interrupted\n", versionOrTag)
		p.errf("      after it started publishing. An UNKNOWN SUBSET of the per-arch\n")
		p.errf("      images and manifests may already be in the registry; this report\n")
		p.errf("      cannot enumerate them. Inspect the %s/* image\n", p.cfg.Project)
		p.errf("      repositories if you need the exact list.\n")
	} else {
		p.errf("  ALREADY PUBLISHED to %s (cannot be rolled back here):\n", dest.Registry)
		p.errf("    * container images for %s (goreleaser publish finished)\n", versionOrTag)

		if st.confirmedRing2 {
			p.errf("    * ring2 chart %s:%s\n", ring2, st.chartVersion)
		}

		// A cancellation can land AFTER the ring3 push confirmed but
		// BEFORE releaseComplete flipped — the release is then fully
		// published and this report must say so, not imply ring3 is
		// missing.
		if st.confirmedRing3 {
			p.errf("    * ring3 chart %s:%s  <- the commit point\n", ring3, st.chartVersion)
		}
	}

	// Started-but-unconfirmed chart pushes are UNKNOWN, not absent: the
	// upload may have landed even though helmctl never returned success.
	if (st.startedRing2 && !st.confirmedRing2) || (st.startedRing3 && !st.confirmedRing3) {
		p.errf("  POSSIBLY PUBLISHED to %s (push interrupted — outcome UNKNOWN,\n", dest.Registry)
		p.errf("  check the registry before any retry):\n")

		if st.startedRing2 && !st.confirmedRing2 {
			p.errf("    * ring2 chart %s:%s\n", ring2, st.chartVersion)
		}

		if st.startedRing3 && !st.confirmedRing3 {
			p.errf("    * ring3 chart %s:%s\n", ring3, st.chartVersion)
		}
	}

	if !st.startedRing2 || !st.startedRing3 {
		p.errf("  NOT published:\n")

		if !st.startedRing2 {
			p.errf("    * ring2 chart %s\n", ring2)
		}

		if !st.startedRing3 {
			p.errf("    * ring3 chart %s  <- the commit point\n", ring3)
		}
	}

	p.errf("\n")

	switch {
	case st.confirmedRing3:
		p.errf("  Ring3 is the commit point and its push CONFIRMED before this run died:\n")
		p.errf("  every artifact of %s is published. The release IS complete —\n", versionOrTag)
		p.errf("  only this run's final bookkeeping was interrupted.\n")
	case st.startedRing3:
		p.errf("  Ring3 is the commit point and its push was interrupted: if it landed,\n")
		p.errf("  the release is complete despite this failure; if it did not, treat\n")
		p.errf("  %s as NOT released. The registry, not this report, holds\n", versionOrTag)
		p.errf("  the answer — check it first.\n")
	default:
		p.errf("  Ring3 is what makes a version look released, and it is missing, so no\n")
		p.errf("  consumer can resolve this release: the images are unreachable without\n")
		p.errf("  ring3's digest-pinned values, and ring2 on its own is just an\n")
		p.errf("  unreferenced infra chart. Treat %s as NOT released.\n", versionOrTag)
	}

	p.errf("\n")
	p.errf("  The stable repositories (images AND charts) are IMMUTABLE_WITH_EXCLUSION\n")
	p.errf("  — only latest* is mutable — so an already-pushed version tag can never\n")
	p.errf("  be overwritten, and re-running this flow for the same tag dies on the\n")
	p.errf("  first already-present artifact. Recovery depends on where this run died:\n")
	p.errf("\n")

	switch {
	case st.confirmedRing3:
		// Ring3 confirmed == the release landed in full; only the flip of
		// releaseComplete (and this run's exit) was interrupted. Advising
		// a ring3 push here would send the operator into the immutable
		// version tag.
		p.errf("  RECOVERY — nothing to push:\n")
		p.errf("  Ring3 (the commit point) was CONFIRMED, so every artifact of\n")
		p.errf("  %s is already in the registry. Do NOT re-push either\n", versionOrTag)
		p.errf("  chart — the immutable version tags make a second push die — and do\n")
		p.errf("  NOT cut a new tag. The release itself is done; only whatever killed\n")
		p.errf("  this run after the final push needs attention.\n")
	case st.publishedImages && st.chartsSelected:
		p.errf("  RECOVERY — push the missing chart(s) by hand, nothing else:\n")
		p.errf("  The images are complete and correct; only chart pushes are missing, and\n")
		p.errf("  a full re-run would die in goreleaser on the immutable image tags. This\n")
		p.errf("  run's validated tarballs are staged in %s — an immutable\n", st.stageDir)
		p.errf("  snapshot of %s (the ring-pair selector named this coherent\n", p.cfg.ChartsOut())
		p.errf("  pair), kept for exactly this recovery; remove the directory once done.\n")
		p.errf("  For a push marked UNKNOWN above, confirm it is really absent from the\n")
		p.errf("  registry first — pushing a chart that already landed dies on the\n")
		p.errf("  immutable version tag. Then push each ring that is missing:\n")

		if !st.confirmedRing2 {
			p.recoveryPushAdvice(ring2, st.infraTgz, st.chartVersion, dest)
		}

		p.recoveryPushAdvice(ring3, st.appTgz, st.chartVersion, dest)
		p.errf("  Ring2 before ring3 (install order). Once ring3 is up the release is\n")
		p.errf("  complete — do NOT cut a new tag for this case.\n")
	case st.publishedImages:
		p.errf("  RECOVERY — complete chart packaging manually from the generated manifest:\n")
		p.errf("  The images are complete and correct, but this run died BEFORE a coherent\n")
		p.errf("  chart pair was packaged and selected, so there is no tarball this report\n")
		p.errf("  can honestly tell you to push. Package both rings from this run's\n")
		p.errf("  manifest (%s; if it is missing, regenerate it first with\n", p.cfg.ManifestPath())
		p.errf("  '%s goreleaser-manifest --goreleaser-dist %s -o %s'):\n",
			p.helmctlDisplay(), p.cfg.DistDir, p.cfg.ManifestPath())

		if p.cfg.Charts.Ring2.VendorDependencies {
			p.errf("      %s dependency update %s\n", p.helmDisplay(), p.cfg.Charts.Ring2.Path)
		}

		version := st.version
		if version == "" {
			version = "<version>"
		}

		p.errf("      %s package --chart %s \\\n", p.helmctlDisplay(), p.cfg.Charts.Ring2.Path)
		p.errf("        --version '%s' --output '%s'\n", version, p.cfg.ChartsOut())
		p.errf("      %s package --chart %s \\\n", p.helmctlDisplay(), p.cfg.Charts.Ring3.Path)
		p.errf("        --manifest '%s' --require-image-digests --output '%s'\n",
			p.cfg.ManifestPath(), p.cfg.ChartsOut())
		p.errf("  then push ring2 before ring3 exactly as the RECOVERY block above would\n")
		p.errf("  (registry %s, repositories %s/*). Do NOT cut\n", dest.Registry, p.cfg.Charts.RepositoryPrefix)
		p.errf("  a new tag: the %s images are already published and immutable.\n", versionOrTag)
	default:
		p.errf("  RECOVERY — cut a new PATCH tag:\n")
		p.errf("  goreleaser died mid-publish, so some of %s's image tags may\n", versionOrTag)
		p.errf("  already exist and can never be overwritten or completed — the version\n")
		p.errf("  is burned. Fix the cause, cut the next PATCH tag, release that. The\n")
		p.errf("  stranded partial artifacts are inert: nothing can reference images\n")
		p.errf("  that ring3 never pointed at.\n")
	}

	p.errf("============================================================================\n")
}

// recoveryPushAdvice renders one manual helmctl push command for the
// partial-publish report. Always names the config's profile: the human
// re-running this on a laptop is never ambient.
func (p *Pipeline) recoveryPushAdvice(name, tgz, version string, dest Destination) {
	p.errf("      %s push --tgz '%s' \\\n", p.helmctlDisplay(), tgz)
	p.errf("        --registry '%s' --repository %s \\\n", dest.Registry, p.cfg.ChartRepository(name))
	p.errf("        --profile '%s' --name %s --version '%s'\n", dest.Profile, name, version)
}
