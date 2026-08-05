package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSelectRingCharts is the table over charts-select.sh's refusals: a
// reused output directory cannot be trusted to hold only one build, and
// a file can lie about being a chart.
func TestSelectRingCharts(t *testing.T) {
	const hint = "rebuild-hint-command"

	cases := []struct {
		name       string
		seed       func(t *testing.T, dir string)
		wantErr    string
		wantStderr []string
	}{
		{
			name: "coherent pair",
			seed: func(t *testing.T, dir string) {
				writeChartTgz(t, filepath.Join(dir, "url-shortener-infra-1.2.3.tgz"), "url-shortener-infra", "1.2.3")
				writeChartTgz(t, filepath.Join(dir, "url-shortener-1.2.3.tgz"), "url-shortener", "1.2.3")
			},
		},
		{
			name: "duplicate ring2",
			seed: func(t *testing.T, dir string) {
				writeChartTgz(t, filepath.Join(dir, "url-shortener-infra-1.2.3.tgz"), "url-shortener-infra", "1.2.3")
				writeChartTgz(t, filepath.Join(dir, "url-shortener-infra-1.2.4.tgz"), "url-shortener-infra", "1.2.4")
				writeChartTgz(t, filepath.Join(dir, "url-shortener-1.2.3.tgz"), "url-shortener", "1.2.3")
			},
			wantErr:    "more than one ring2 chart",
			wantStderr: []string{"more than one ring2 chart", "url-shortener-infra-1.2.3.tgz", "url-shortener-infra-1.2.4.tgz", hint},
		},
		{
			name: "duplicate ring3",
			seed: func(t *testing.T, dir string) {
				writeChartTgz(t, filepath.Join(dir, "url-shortener-infra-1.2.3.tgz"), "url-shortener-infra", "1.2.3")
				writeChartTgz(t, filepath.Join(dir, "url-shortener-1.2.3.tgz"), "url-shortener", "1.2.3")
				writeChartTgz(t, filepath.Join(dir, "url-shortener-1.2.4.tgz"), "url-shortener", "1.2.4")
			},
			wantErr:    "more than one ring3 chart",
			wantStderr: []string{"more than one ring3 chart"},
		},
		{
			name: "missing ring2",
			seed: func(t *testing.T, dir string) {
				writeChartTgz(t, filepath.Join(dir, "url-shortener-1.2.3.tgz"), "url-shortener", "1.2.3")
			},
			wantErr:    "incomplete chart set",
			wantStderr: []string{"missing:", "url-shortener-infra-*.tgz (ring2)"},
		},
		{
			name: "missing ring3",
			seed: func(t *testing.T, dir string) {
				writeChartTgz(t, filepath.Join(dir, "url-shortener-infra-1.2.3.tgz"), "url-shortener-infra", "1.2.3")
			},
			wantErr:    "incomplete chart set",
			wantStderr: []string{"url-shortener-*.tgz (ring3)"},
		},
		{
			name:       "empty directory",
			seed:       func(*testing.T, string) {},
			wantErr:    "incomplete chart set",
			wantStderr: []string{"missing:"},
		},
		{
			name: "unexpected tarball name",
			seed: func(t *testing.T, dir string) {
				writeChartTgz(t, filepath.Join(dir, "surprise-9.9.9.tgz"), "surprise", "9.9.9")
			},
			wantErr:    "unexpected chart tgz",
			wantStderr: []string{"unexpected chart tgz 'surprise-9.9.9'"},
		},
		{
			name: "version mismatch between rings",
			seed: func(t *testing.T, dir string) {
				writeChartTgz(t, filepath.Join(dir, "url-shortener-infra-1.2.3.tgz"), "url-shortener-infra", "1.2.3")
				writeChartTgz(t, filepath.Join(dir, "url-shortener-2.0.0.tgz"), "url-shortener", "2.0.0")
			},
			wantErr:    "incoherent chart pair",
			wantStderr: []string{"the rings disagree on the version", "1.2.3", "2.0.0"},
		},
		{
			name: "unversioned tarball",
			seed: func(t *testing.T, dir string) {
				writeChartTgz(t, filepath.Join(dir, "url-shortener-infra-.tgz"), "url-shortener-infra", "")
				writeChartTgz(t, filepath.Join(dir, "url-shortener-.tgz"), "url-shortener", "")
			},
			wantErr:    "unversioned chart tarball",
			wantStderr: []string{"unversioned chart tarball"},
		},
		{
			// A file can lie: the filename claims ring3 at 1.2.3, the
			// Chart.yaml inside carries something else entirely.
			name: "chart.yaml name impostor",
			seed: func(t *testing.T, dir string) {
				writeChartTgz(t, filepath.Join(dir, "url-shortener-infra-1.2.3.tgz"), "url-shortener-infra", "1.2.3")
				writeChartTgz(t, filepath.Join(dir, "url-shortener-1.2.3.tgz"), "totally-different", "1.2.3")
			},
			wantErr: "does not match its filename",
			wantStderr: []string{
				"filename claims:  name 'url-shortener', version '1.2.3'",
				"Chart.yaml says:  name 'totally-different', version '1.2.3'",
			},
		},
		{
			name: "chart.yaml version impostor",
			seed: func(t *testing.T, dir string) {
				writeChartTgz(t, filepath.Join(dir, "url-shortener-infra-1.2.3.tgz"), "url-shortener-infra", "9.9.9")
				writeChartTgz(t, filepath.Join(dir, "url-shortener-1.2.3.tgz"), "url-shortener", "1.2.3")
			},
			wantErr:    "does not match its filename",
			wantStderr: []string{"Chart.yaml says:  name 'url-shortener-infra', version '9.9.9'"},
		},
		{
			name: "tarball without Chart.yaml",
			seed: func(t *testing.T, dir string) {
				writeChartTgz(t, filepath.Join(dir, "url-shortener-infra-1.2.3.tgz"), "url-shortener-infra", "1.2.3")
				writeTgz(t, filepath.Join(dir, "url-shortener-1.2.3.tgz"), map[string]string{"README.md": "not a chart"})
			},
			wantErr:    "not a packaged helm chart",
			wantStderr: []string{"no Chart.yaml at the archive root"},
		},
		{
			name: "not a gzip archive at all",
			seed: func(t *testing.T, dir string) {
				writeChartTgz(t, filepath.Join(dir, "url-shortener-infra-1.2.3.tgz"), "url-shortener-infra", "1.2.3")
				require.NoError(t, os.WriteFile(filepath.Join(dir, "url-shortener-1.2.3.tgz"), []byte("plain text"), 0o644))
			},
			wantErr:    "not a packaged helm chart",
			wantStderr: []string{"no Chart.yaml at the archive root"},
		},
		{
			// A directory named *.tgz is not a chart: skipped, and the
			// completeness check reports the ring as missing.
			name: "directory named like a tarball",
			seed: func(t *testing.T, dir string) {
				writeChartTgz(t, filepath.Join(dir, "url-shortener-infra-1.2.3.tgz"), "url-shortener-infra", "1.2.3")
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "url-shortener-1.2.3.tgz"), 0o755))
			},
			wantErr:    "incomplete chart set",
			wantStderr: []string{"url-shortener-*.tgz (ring3)"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _, _, stderr := newTestPipeline(t)
			dir := t.TempDir()
			tc.seed(t, dir)

			sel, err := p.selectRingCharts(dir, hint, "display/charts")

			if tc.wantErr == "" {
				require.NoError(t, err)
				assert.Equal(t, "1.2.3", sel.Version)
				assert.Equal(t, filepath.Join(dir, "url-shortener-infra-1.2.3.tgz"), sel.InfraTgz)
				assert.Equal(t, filepath.Join(dir, "url-shortener-1.2.3.tgz"), sel.AppTgz)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)

			for _, want := range tc.wantStderr {
				assert.Contains(t, stderr.String(), want)
			}
		})
	}
}

// TestSelectNamesDisplayDir proves errors name the REAL output dir, not
// the transient stage that is deleted the moment the flow exits.
func TestSelectNamesDisplayDir(t *testing.T) {
	p, _, _, stderr := newTestPipeline(t)
	stage := t.TempDir()

	_, err := p.selectRingCharts(stage, "hint", "dist/url-shortener/charts")
	require.Error(t, err)
	assert.Contains(t, stderr.String(), "dist/url-shortener/charts")
	assert.NotContains(t, stderr.String(), stage)
}
