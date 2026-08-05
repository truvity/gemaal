package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// PushPreview pushes the packaged ring charts to the PREVIEW registry
// (the port of charts-push.sh). Dev-loop convenience only: preview
// pushes are how test charts get shared. No knobs — preview is
// hardwired.
//
// Stable charts are NEVER pushed here: the stable release is one gated
// flow (ReleaseStable) that builds and pushes in the same run. This flow
// refuses a stable-stamped charts directory accordingly — the ring3
// chart carries digest-pinned image references that INCLUDE the registry
// host, baked in at package time, and pushing a chart built for the
// other registry would hand consumers image refs in the wrong account.
func (p *Pipeline) PushPreview(ctx context.Context) error {
	if err := p.resolveRoot(ctx); err != nil {
		return err
	}

	// Dev loop == preview ring, always.
	dest := p.cfg.Registries.Preview
	env := p.destinationEnv(dest, false)
	chartsOut := p.cfg.ChartsOut()
	rebuildHint := p.cfg.Hints.Snapshot

	// 1. Freeze the build output BEFORE looking at any of it — under the
	// shared chart lock. Two layers make the freeze real: the flock (a
	// bare copy is not atomic — without the lock a concurrent build could
	// tear the stage in exactly the way that defeats staging) and the
	// BLIND copy of the ENTIRE directory into a private temp dir
	// (validate-then-copy from the shared dir would be a TOCTOU hole).
	release, err := p.acquireChartLock(ctx)
	if err != nil {
		return err
	}
	defer release()

	if _, err := os.Stat(p.abs(chartsOut)); err != nil {
		p.errf("error: %s does not exist — no packaged charts to push\n", chartsOut)
		p.errf("hint: rebuild with '%s'\n", rebuildHint)

		return fmt.Errorf("%s does not exist", chartsOut)
	}

	stage, err := p.stageCharts()
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()

	// 2. Select the ring pair FROM THE SNAPSHOT and prove it is coherent:
	// exactly one tarball per ring, both rings on the SAME version, each
	// tarball being the chart its filename claims. Error messages keep
	// naming the real output dir — the stage is a verbatim copy of it,
	// and it is the real dir that a rebuild repopulates.
	sel, err := p.selectRingCharts(stage, rebuildHint, chartsOut)
	if err != nil {
		return err
	}

	// 3. Provenance: the charts must have been PACKAGED for preview.
	stamped, ok, err := readStamp(filepath.Join(stage, stampFileName))
	if err != nil {
		return err
	}

	if !ok {
		p.errf("error: no %s — the charts in %s have unknown provenance\n", p.cfg.StampPath(), chartsOut)
		p.errf("hint: rebuild with '%s'\n", rebuildHint)

		return errors.New("charts have unknown provenance")
	}

	if stamped != StampPreview {
		p.errf("error: packaged charts are stamped '%s', but this is the preview-only push.\n", stamped)
		p.errf("       The ring3 chart bakes digest-pinned image refs (registry included) at package\n")
		p.errf("       time, so publishing them here would point consumers at the '%s' registry.\n", stamped)
		p.errf("       A stable chart set is pushed by its own gated build\n")
		p.errf("       ('%s') and never passes through this flow.\n", p.cfg.Hints.Stable)
		p.errf("hint: rebuild for preview with '%s'\n", rebuildHint)

		return fmt.Errorf("charts are stamped %q, not %q", stamped, StampPreview)
	}

	// Stage + validation done — the shared dir no longer matters to this
	// run. Drop the lock BEFORE the login: the pushes read only the
	// stage, and holding the lock through registry uploads would stall a
	// concurrent build for no protection at all.
	release()

	// Every check above ran against the frozen snapshot, and all of it
	// before this login: a bad chart directory never reaches a credential.
	if err := p.ecrLogin(ctx, env, dest); err != nil {
		return err
	}

	// 4. Push ring2 first, then ring3 — least-consumable artifact first
	// (a lone ring3 is the broken half; a lone ring2 is just an
	// unreferenced infra chart). Two pushes are not one transaction: if
	// the ring3 push fails, ring2 stays published at that version —
	// re-running republishes the same coherent pair.
	if err := p.pushChart(ctx, env, dest, p.cfg.Charts.Ring2.Name, sel.InfraTgz, sel.Version); err != nil {
		return err
	}

	if err := p.pushChart(ctx, env, dest, p.cfg.Charts.Ring3.Name, sel.AppTgz, sel.Version); err != nil {
		return err
	}

	p.log.Info("preview charts pushed",
		slog.String("version", sel.Version),
		slog.String("registry", dest.Registry))

	return nil
}
