package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// lockRetryDelay is how often a blocked contender re-attempts the flock.
const lockRetryDelay = 100 * time.Millisecond

// acquireChartLock takes the ONE exclusive lock serializing every writer
// and reader of the packaged charts directory (the port of
// charts-lock.sh, on github.com/gofrs/flock so the same code covers
// Linux and macOS without a flock(1) binary).
//
// WHY: the charts directory is SHARED build output, and both push flows
// freeze it with a blind copy into a private stage before validating
// anything. The copy makes the VALIDATION race-free, but the copy itself
// is not atomic: a build mutating the directory mid-copy can tear the
// stage (half old build, half new). This lock is what makes
// stage-then-validate a true point-in-time snapshot:
//
//   - WRITERS (snapshot, release-stable) hold it exclusively across
//     their ENTIRE mutation window — cleanup through goreleaser through
//     packaging to the stamp write — so the directory is only ever
//     observed between builds, never mid-build;
//   - PUSHERS (push-preview, release-stable's pre-push staging) hold it
//     across stage-copy + validation, and drop it BEFORE any credential
//     use — the registry pushes read only the private stage and must not
//     make a concurrent build wait out network uploads.
//
// THE INVARIANT: one lock, held for the whole mutation window; the
// lockfile lives outside every cleaned path (config validation enforces
// it). flock is inode-based, so a lock file that anything deletes
// mid-hold excludes nobody — `goreleaser --clean` wipes its dist dir
// wholesale, which is why the lock sits NEXT TO that directory, never
// inside it: a single acquisition can — and does — span the entire
// window, goreleaser included.
//
// The returned release func is idempotent; the lock also drops on
// process exit, so error paths need no explicit release.
func (p *Pipeline) acquireChartLock(ctx context.Context) (release func(), err error) {
	lockPath := p.abs(p.cfg.Lock.File)

	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("prepare chart lock dir: %w", err)
	}

	timeout := p.cfg.Lock.Timeout.Std()

	lockCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	fl := flock.New(lockPath)

	locked, lockErr := fl.TryLockContext(lockCtx, lockRetryDelay)
	if locked {
		var once sync.Once

		return func() { once.Do(func() { _ = fl.Unlock() }) }, nil
	}

	if lockErr != nil && !errors.Is(lockErr, context.DeadlineExceeded) {
		return nil, fmt.Errorf("take chart lock %s: %w", p.cfg.Lock.File, lockErr)
	}

	p.errf("error: another build/push holds the chart lock (%s) — gave up after %s.\n", p.cfg.Lock.File, timeout)
	p.errf("       A concurrent snapshot, preview push or stable release owns\n")
	p.errf("       %s right now. Let it finish and re-run.\n", p.cfg.ChartsOut())

	return nil, fmt.Errorf("chart lock %s unavailable after %s", p.cfg.Lock.File, timeout)
}
