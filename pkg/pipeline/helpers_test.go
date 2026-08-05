package pipeline

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// testConfigYAML is bar's url-shortener shape with example registries —
// the config instance the flows are exercised against.
const testConfigYAML = `
project: url-shortener
awsRegion: eu-central-1
registries:
  preview: {registry: preview.example.com, profile: preview@power}
  stable: {registry: stable.example.com, profile: stable@power}
charts:
  ring2:
    name: url-shortener-infra
    path: url-shortener/charts/url-shortener-infra
    vendorDependencies: true
  ring3:
    name: url-shortener
    path: url-shortener/charts/url-shortener
commands:
  preBuild:
    - [moon, run, url-shortener:build/app/web, url-shortener:build/app/log]
`

func testConfig(t *testing.T) *Config {
	t.Helper()

	cfg, err := Parse([]byte(testConfigYAML))
	require.NoError(t, err)

	return cfg
}

// stubResult scripts one matched invocation: captured stdout, returned
// error, and an optional filesystem effect (e.g. "goreleaser wrote the
// dist dir").
type stubResult struct {
	out    string
	err    error
	effect func(c Command) error
}

type stubRule struct {
	prefix string
	res    stubResult
}

// stubRunner is the injected executor: rules match on a prefix of the
// space-joined argv, newest rule first (so tests override defaults).
// Unmatched commands succeed with empty output.
type stubRunner struct {
	mu    sync.Mutex
	rules []stubRule
	calls []Command
}

func (s *stubRunner) on(prefix string, res stubResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append([]stubRule{{prefix, res}}, s.rules...)
}

func (s *stubRunner) exec(c Command) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	rules := s.rules
	s.mu.Unlock()

	joined := strings.Join(c.Argv, " ")
	for _, r := range rules {
		if strings.HasPrefix(joined, r.prefix) {
			if r.res.effect != nil {
				if err := r.res.effect(c); err != nil {
					return "", err
				}
			}

			return r.res.out, r.res.err
		}
	}

	return "", nil
}

// Run implements Runner.
func (s *stubRunner) Run(_ context.Context, c Command) error {
	_, err := s.exec(c)

	return err
}

// Output implements Runner.
func (s *stubRunner) Output(_ context.Context, c Command) (string, error) {
	return s.exec(c)
}

// joinedCalls returns every recorded invocation as a space-joined string.
func (s *stubRunner) joinedCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, len(s.calls))
	for i, c := range s.calls {
		out[i] = strings.Join(c.Argv, " ")
	}

	return out
}

// find returns the index of the first call matching the prefix, or -1.
func (s *stubRunner) find(prefix string) int {
	for i, c := range s.joinedCalls() {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}

	return -1
}

func (s *stubRunner) called(prefix string) bool { return s.find(prefix) >= 0 }

// call returns the recorded Command at the first call matching prefix.
func (s *stubRunner) call(t *testing.T, prefix string) Command {
	t.Helper()

	idx := s.find(prefix)
	require.GreaterOrEqual(t, idx, 0, "no call matching %q in %v", prefix, s.joinedCalls())

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls[idx]
}

// newTestPipeline wires a Pipeline against a temp git root and a stub
// runner with the root resolution pre-scripted.
func newTestPipeline(t *testing.T) (p *Pipeline, s *stubRunner, root string, stderr *bytes.Buffer) {
	t.Helper()

	root = t.TempDir()
	s = &stubRunner{}
	s.on("git rev-parse --show-toplevel", stubResult{out: root + "\n"})

	stderr = &bytes.Buffer{}
	p = New(testConfig(t), s, slog.New(slog.DiscardHandler), stderr)

	return p, s, root, stderr
}

// writeTgz writes a gzipped tarball with the given members.
func writeTgz(t *testing.T, path string, files map[string]string) {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for name, content := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}))

		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}

	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
}

// writeChartTgz writes a valid packaged chart whose Chart.yaml carries
// the given identity.
func writeChartTgz(t *testing.T, path, chartName, chartVersion string) {
	t.Helper()

	writeTgz(t, path, map[string]string{
		chartName + "/Chart.yaml": fmt.Sprintf("apiVersion: v2\nname: %s\nversion: %s\n", chartName, chartVersion),
	})
}

// seedCharts populates the shared charts output with a coherent ring
// pair and a stamp.
func seedCharts(t *testing.T, root string, cfg *Config, version, stamp string) {
	t.Helper()

	chartsOut := filepath.Join(root, filepath.FromSlash(cfg.ChartsOut()))
	writeChartTgz(t, filepath.Join(chartsOut, cfg.Charts.Ring2.Name+"-"+version+".tgz"), cfg.Charts.Ring2.Name, version)
	writeChartTgz(t, filepath.Join(chartsOut, cfg.Charts.Ring3.Name+"-"+version+".tgz"), cfg.Charts.Ring3.Name, version)

	if stamp != "" {
		require.NoError(t, os.WriteFile(filepath.Join(chartsOut, stampFileName), []byte(stamp+"\n"), 0o644))
	}
}

// assertOnlyGitRan asserts a gate failure stopped the flow before any
// build tool or credential use: every recorded call is git.
func assertOnlyGitRan(t *testing.T, s *stubRunner) {
	t.Helper()

	for _, c := range s.joinedCalls() {
		require.True(t, strings.HasPrefix(c, "git "), "non-git command ran despite gate failure: %s", c)
	}
}
