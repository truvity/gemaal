package pipeline

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/gemaal/pkg/config"
)

// TestChartLockExclusion contends two in-process pipelines on one
// lockfile: the second acquisition must time out while the first holds,
// and succeed once released.
func TestChartLockExclusion(t *testing.T) {
	root := t.TempDir()

	newLockPipeline := func(timeout time.Duration) (*Pipeline, *bytes.Buffer) {
		cfg := testConfig(t)
		cfg.Lock.Timeout = config.Duration(timeout)

		stderr := &bytes.Buffer{}
		p := New(cfg, &stubRunner{}, slog.New(slog.DiscardHandler), stderr)
		p.root = root

		return p, stderr
	}

	holder, _ := newLockPipeline(time.Second)
	contender, contenderStderr := newLockPipeline(300 * time.Millisecond)

	release, err := holder.acquireChartLock(context.Background())
	require.NoError(t, err)

	// While held: the contender gives up after its timeout and explains
	// who owns the directory.
	_, err = contender.acquireChartLock(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chart lock")
	assert.Contains(t, contenderStderr.String(), "another build/push holds the chart lock")
	assert.Contains(t, contenderStderr.String(), "Let it finish and re-run")

	// Released: the contender acquires immediately.
	release()

	release2, err := contender.acquireChartLock(context.Background())
	require.NoError(t, err)
	release2()

	// The release func is idempotent.
	release()
	release2()
}
