package pipeline

import (
	"context"
	"log/slog"
	"path/filepath"
)

// Snapshot is the DEV LOOP build (the port of artifacts-snapshot.sh).
// Preview registry only. No knobs, no gates: it builds whatever the
// working tree holds RIGHT NOW — dirty, uncommitted, untagged — and
// publishes to the preview account so the packaged charts are actually
// deployable. Speed is the point.
//
// The stable release is the OPPOSITE workflow (ReleaseStable): clean
// tree, tagged tip of the release branch, five gates. The two
// deliberately share no entrypoint and no knob.
//
// Steps:
//
//  0. drop the previous run's packaged charts + .release-type stamp,
//     before anything can fail — a half-finished build must leave
//     nothing a push flow would publish;
//
//  1. goreleaser --snapshot from the git root, on the OSS build.
//     DIRTY-TREE SUPPORT: the git dirty-state check lives in
//     goreleaser's validate step, so --skip=validate is what accepts an
//     uncommitted tree — the Pro-only --nightly implied it, and that is
//     one of the two things it was here for.
//
//     The other was publishing. --snapshot implies --skip=publish, so
//     goreleaser builds the images and pushes nothing, which alone would
//     leave the digest-pinned charts referencing images that never
//     reached a registry. Both image kinds are therefore pushed by this
//     pipeline: dockers_v2 by publishImages, kos by publishKoImages.
//     With those two, nothing Pro-only is left in the flow;
//
//  2. helmctl goreleaser-manifest — digest-pinned release manifest;
//
//  3. helmctl package for every chart the tag produces;
//
//  4. a `preview` .release-type stamp next to the packaged charts, so
//     the preview push can refuse charts built for the other registry.
//
// The ENTIRE build — step 0's cleanup through the stamp write —
// goreleaser included, runs under one acquisition of the exclusive chart
// lock: a concurrent push stages either the previous complete build or
// this one — never a torn mid-build read, and never the empty directory
// between wipe and repackage.
func (p *Pipeline) Snapshot(ctx context.Context) error {
	if err := p.resolveRoot(ctx); err != nil {
		return err
	}

	// Dev loop == preview ring, always.
	dest := p.cfg.Registries.Preview
	env := p.destinationEnv(dest, true)

	p.log.Info("building dev-loop artifacts (preview)",
		slog.String("project", p.cfg.Project),
		slog.String("registry", dest.Registry))

	// WRITER lock: held from the cleanup through the stamp write — one
	// acquisition for the whole mutation window.
	release, err := p.acquireChartLock(ctx)
	if err != nil {
		return err
	}
	defer release()

	// 0. Invalidate the previous build's output BEFORE the first build
	// step: a build that dies mid-way must not leave the PREVIOUS run's
	// tarballs next to a stamp that still vouches for them.
	if err := p.cleanChartsOutput(); err != nil {
		return err
	}

	// 1. Images. goreleaser runs from the git root (monorepo tag_prefix,
	// per-project dist) and builds only: --skip=validate accepts the
	// dirty tree, and both image kinds are skipped here and pushed
	// below. dockers_v2 goes by digest and is tagged once (the shape an
	// immutable registry needs; the preview registry gets it for
	// parity), and kos is pushed by ko itself, which writes one index
	// under one tag.
	if err := p.run(ctx, env, p.cfg.Commands.Goreleaser,
		"release", "--snapshot", "--clean", "--skip=validate,docker,ko",
		"-f", p.cfg.GoreleaserConfig); err != nil {
		return err
	}

	if _, err := p.publishImages(ctx, env, true); err != nil {
		return err
	}

	if _, err := p.publishKoImages(ctx, env, true); err != nil {
		return err
	}

	// 2. Release manifest (version + digest-pinned image values).
	if err := p.run(ctx, env, p.cfg.Commands.Helmctl,
		"goreleaser-manifest", "--goreleaser-dist", p.cfg.DistDir, "-o", p.cfg.ManifestPath()); err != nil {
		return err
	}

	version, err := p.manifestVersion()
	if err != nil {
		return err
	}

	// 3. Every chart of the tag. goreleaser --clean removed the dist dir
	// above; packageCharts recreates the output dir. The writer lock is
	// STILL HELD — the wipe cannot touch its inode (the lockfile lives
	// outside the cleaned dir), so no re-acquisition happens anywhere here.
	if err := p.packageCharts(ctx, env, version); err != nil {
		return err
	}

	// 4. Push the packaged charts to the preview registry as OCI, under
	// {registry}/{project}/charts/{name}:{version}. This is what makes a
	// build consumable OUTSIDE the repository that made it -- a consumer
	// suite (the SDK e2e repos, DMS-65) installs the ring pair by
	// version instead of rebuilding core with its own goreleaser key.
	// helm rides the same docker credential config the image pushes just
	// used, so no separate login. The ECR repositories themselves are
	// gitops-created (an OCI push cannot create one); a missing repo
	// fails HERE, loudly, next to the name it needs.
	if err := p.pushCharts(ctx, env, dest); err != nil {
		return err
	}

	// 5. Provenance stamp: the dev loop only ever produces preview
	// charts. The stamp completes the chart output — end of the mutation
	// window, so the lock drops with it.
	if err := p.writeStamp(StampPreview); err != nil {
		return err
	}

	release()

	charts, _ := filepath.Glob(filepath.Join(p.abs(p.cfg.ChartsOut()), "*.tgz"))
	for i, c := range charts {
		charts[i] = filepath.Base(c)
	}

	p.log.Info("dev-loop build complete (preview)",
		slog.String("version", version),
		slog.Any("charts", charts))

	return nil
}
