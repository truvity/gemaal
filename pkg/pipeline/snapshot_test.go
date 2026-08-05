package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubBuildEffects scripts the build tools' filesystem effects:
// goreleaser --clean wipes the dist dir, the manifest generator writes
// the version document, and each package invocation drops its tarball.
func stubBuildEffects(t *testing.T, s *stubRunner, root, goreleaserPrefix, version string) {
	t.Helper()

	dist := filepath.Join(root, "dist", "url-shortener")
	chartsOut := filepath.Join(dist, "charts")

	s.on(goreleaserPrefix, stubResult{effect: func(Command) error {
		return os.RemoveAll(dist)
	}})

	s.on("go tool helmctl goreleaser-manifest", stubResult{effect: func(Command) error {
		if err := os.MkdirAll(dist, 0o755); err != nil {
			return err
		}

		return os.WriteFile(filepath.Join(dist, "chart-manifest.yaml"), []byte("version: "+version+"\nimages: {}\n"), 0o644)
	}})

	s.on("go tool helmctl package --chart url-shortener/charts/url-shortener-infra", stubResult{effect: func(Command) error {
		writeChartTgz(t, filepath.Join(chartsOut, "url-shortener-infra-"+version+".tgz"), "url-shortener-infra", version)

		return nil
	}})

	s.on("go tool helmctl package --chart url-shortener/charts/url-shortener --manifest", stubResult{effect: func(Command) error {
		writeChartTgz(t, filepath.Join(chartsOut, "url-shortener-"+version+".tgz"), "url-shortener", version)

		return nil
	}})
}

func TestSnapshotHappyPath(t *testing.T) {
	p, s, root, _ := newTestPipeline(t)
	chartsOut := filepath.Join(root, "dist", "url-shortener", "charts")

	// A previous build's leftovers: stale tarball + a stamp that still
	// vouches for it.
	seedCharts(t, root, p.cfg, "0.0.9", StampStable)

	stubBuildEffects(t, s, root, "goreleaser release --nightly --clean -f url-shortener/.goreleaser.yaml", "1.2.3")

	require.NoError(t, p.Snapshot(context.Background()))

	// The build is nightly (dirty-capable, still publishing) — never the
	// full-validation invocation.
	require.True(t, s.called("goreleaser release --nightly"))

	// Ordering is load-bearing: images, then manifest, then vendoring,
	// then ring2, then ring3.
	gorel := s.find("goreleaser release --nightly")
	manifest := s.find("go tool helmctl goreleaser-manifest")
	depUpdate := s.find("helm dependency update url-shortener/charts/url-shortener-infra")
	ring2 := s.find("go tool helmctl package --chart url-shortener/charts/url-shortener-infra")
	ring3 := s.find("go tool helmctl package --chart url-shortener/charts/url-shortener --manifest")

	for _, idx := range []int{gorel, manifest, depUpdate, ring2, ring3} {
		require.GreaterOrEqual(t, idx, 0, "missing expected call; calls: %v", s.joinedCalls())
	}

	assert.Less(t, gorel, manifest)
	assert.Less(t, manifest, depUpdate)
	assert.Less(t, depUpdate, ring2)
	assert.Less(t, ring2, ring3)

	// The whole workflow is preview-wired: registry + profile from the
	// preview pair, KO_DOCKER_REPO derived from it.
	env := s.call(t, "goreleaser release --nightly").Env
	assert.Contains(t, env, "REGISTRY=preview.example.com")
	assert.Contains(t, env, "AWS_PROFILE=preview@power")
	assert.Contains(t, env, "AWS_REGION=eu-central-1")
	assert.Contains(t, env, "KO_DOCKER_REPO=preview.example.com/url-shortener")

	// The stale pair is gone, the new pair is stamped `preview`.
	assert.NoFileExists(t, filepath.Join(chartsOut, "url-shortener-infra-0.0.9.tgz"))
	assert.FileExists(t, filepath.Join(chartsOut, "url-shortener-infra-1.2.3.tgz"))
	assert.FileExists(t, filepath.Join(chartsOut, "url-shortener-1.2.3.tgz"))

	stamp, err := os.ReadFile(filepath.Join(chartsOut, stampFileName))
	require.NoError(t, err)
	assert.Equal(t, "preview\n", string(stamp))

	// The dev loop builds and packages — it never logs in or pushes.
	assert.False(t, s.called("aws"))
	assert.False(t, s.called("helm registry login"))
	assert.False(t, s.called("go tool helmctl push"))
}

// TestSnapshotCleansBeforeBuild proves step 0: a build that dies in
// goreleaser leaves neither the previous run's tarballs nor a stamp that
// would vouch for them.
func TestSnapshotCleansBeforeBuild(t *testing.T) {
	p, s, root, _ := newTestPipeline(t)
	chartsOut := filepath.Join(root, "dist", "url-shortener", "charts")

	seedCharts(t, root, p.cfg, "0.0.9", StampPreview)

	s.on("goreleaser release --nightly", stubResult{err: errors.New("boom")})

	require.Error(t, p.Snapshot(context.Background()))

	assert.NoFileExists(t, filepath.Join(chartsOut, "url-shortener-infra-0.0.9.tgz"))
	assert.NoFileExists(t, filepath.Join(chartsOut, "url-shortener-0.0.9.tgz"))
	assert.NoFileExists(t, filepath.Join(chartsOut, stampFileName))
	assert.False(t, s.called("go tool helmctl"))
}

// TestSnapshotRefusesManifestWithoutVersion ports the manifest version
// check: no version, no charts, no stamp.
func TestSnapshotRefusesManifestWithoutVersion(t *testing.T) {
	p, s, root, stderr := newTestPipeline(t)
	dist := filepath.Join(root, "dist", "url-shortener")

	s.on("go tool helmctl goreleaser-manifest", stubResult{effect: func(Command) error {
		if err := os.MkdirAll(dist, 0o755); err != nil {
			return err
		}

		return os.WriteFile(filepath.Join(dist, "chart-manifest.yaml"), []byte("images: {}\n"), 0o644)
	}})

	err := p.Snapshot(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no version in")
	assert.Contains(t, stderr.String(), "no version in dist/url-shortener/chart-manifest.yaml")
	assert.False(t, s.called("go tool helmctl package"))
	assert.NoFileExists(t, filepath.Join(dist, "charts", stampFileName))
}
