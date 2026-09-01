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

// TestSelectExtraCharts is the same table for a config whose tag also
// produces a chart with no ring semantics. The extra's name begins with
// ring3's, so the happy case alone proves the scan files each tarball
// under the LONGEST matching chart name — a first-match-wins scan reads
// url-shortener-broker-1.2.3 as a second ring3 tarball.
func TestSelectExtraCharts(t *testing.T) {
	const hint = "rebuild-hint-command"

	seedRings := func(t *testing.T, dir string) {
		t.Helper()

		writeChartTgz(t, filepath.Join(dir, "url-shortener-infra-1.2.3.tgz"), "url-shortener-infra", "1.2.3")
		writeChartTgz(t, filepath.Join(dir, "url-shortener-1.2.3.tgz"), "url-shortener", "1.2.3")
	}

	cases := []struct {
		name       string
		seed       func(t *testing.T, dir string)
		wantErr    string
		wantStderr []string
	}{
		{
			name: "coherent set",
			seed: func(t *testing.T, dir string) {
				seedRings(t, dir)
				writeChartTgz(t, filepath.Join(dir, "url-shortener-broker-1.2.3.tgz"), "url-shortener-broker", "1.2.3")
			},
		},
		{
			name:       "missing extra",
			seed:       seedRings,
			wantErr:    "incomplete chart set",
			wantStderr: []string{"missing:", "url-shortener-broker-*.tgz (url-shortener-broker)"},
		},
		{
			name: "duplicate extra",
			seed: func(t *testing.T, dir string) {
				seedRings(t, dir)
				writeChartTgz(t, filepath.Join(dir, "url-shortener-broker-1.2.3.tgz"), "url-shortener-broker", "1.2.3")
				writeChartTgz(t, filepath.Join(dir, "url-shortener-broker-1.2.4.tgz"), "url-shortener-broker", "1.2.4")
			},
			wantErr: "more than one url-shortener-broker chart",
			wantStderr: []string{
				"more than one url-shortener-broker chart",
				"url-shortener-broker-1.2.3.tgz",
				"url-shortener-broker-1.2.4.tgz",
			},
		},
		{
			// The extra shares the tag, so it shares the version. One that
			// does not is a leftover wearing this release's name.
			name: "extra from another build",
			seed: func(t *testing.T, dir string) {
				seedRings(t, dir)
				writeChartTgz(t, filepath.Join(dir, "url-shortener-broker-2.0.0.tgz"), "url-shortener-broker", "2.0.0")
			},
			wantErr: "incoherent chart set",
			wantStderr: []string{
				"url-shortener-broker does not carry the release version",
				"rings:  1.2.3",
				"url-shortener-broker: 2.0.0",
			},
		},
		{
			// Filenames are trusted only until identity verification, and
			// an extra is verified like every other chart.
			name: "extra impostor",
			seed: func(t *testing.T, dir string) {
				seedRings(t, dir)
				writeChartTgz(t, filepath.Join(dir, "url-shortener-broker-1.2.3.tgz"), "something-else", "1.2.3")
			},
			wantErr: "does not match its filename",
			wantStderr: []string{
				"filename claims:  name 'url-shortener-broker', version '1.2.3'",
				"Chart.yaml says:  name 'something-else', version '1.2.3'",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _, _, stderr := newExtraTestPipeline(t)
			dir := t.TempDir()
			tc.seed(t, dir)

			sel, err := p.selectRingCharts(dir, hint, "display/charts")

			if tc.wantErr == "" {
				require.NoError(t, err)
				assert.Equal(t, "1.2.3", sel.Version)
				assert.Equal(t, filepath.Join(dir, "url-shortener-infra-1.2.3.tgz"), sel.InfraTgz)
				assert.Equal(t, filepath.Join(dir, "url-shortener-1.2.3.tgz"), sel.AppTgz)
				assert.Equal(t, []chartTarball{{
					Name: "url-shortener-broker",
					Tgz:  filepath.Join(dir, "url-shortener-broker-1.2.3.tgz"),
				}}, sel.Extra)

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

// TestSelectWithoutExtraChartsSelectsNothingExtra is the compatibility
// pin: a config that names no extra charts must select exactly the ring
// pair it always did — no phantom entry, no changed refusal.
func TestSelectWithoutExtraChartsSelectsNothingExtra(t *testing.T) {
	p, _, _, _ := newTestPipeline(t)
	dir := t.TempDir()

	writeChartTgz(t, filepath.Join(dir, "url-shortener-infra-1.2.3.tgz"), "url-shortener-infra", "1.2.3")
	writeChartTgz(t, filepath.Join(dir, "url-shortener-1.2.3.tgz"), "url-shortener", "1.2.3")

	sel, err := p.selectRingCharts(dir, "hint", "display/charts")
	require.NoError(t, err)
	assert.Empty(t, sel.Extra)
}

// TestSelectPrefersTheLongestChartName pins the matching rule itself.
// Chart names of one repo prefix each other, and a first-match-wins scan
// files the longer chart's tarball under the shorter name — here as a
// SECOND ring2 tarball, at a version nobody built.
func TestSelectPrefersTheLongestChartName(t *testing.T) {
	cfg := testConfig(t)
	cfg.Charts.Extra = []Chart{{Name: "url-shortener-infra-jobs", Path: "url-shortener/charts/jobs"}}
	require.NoError(t, cfg.validate())

	p, _, _, _ := newPipelineFor(t, cfg)
	dir := t.TempDir()

	writeChartTgz(t, filepath.Join(dir, "url-shortener-infra-1.2.3.tgz"), "url-shortener-infra", "1.2.3")
	writeChartTgz(t, filepath.Join(dir, "url-shortener-1.2.3.tgz"), "url-shortener", "1.2.3")
	writeChartTgz(t, filepath.Join(dir, "url-shortener-infra-jobs-1.2.3.tgz"), "url-shortener-infra-jobs", "1.2.3")

	sel, err := p.selectRingCharts(dir, "hint", "display/charts")
	require.NoError(t, err)
	assert.Equal(t, "1.2.3", sel.Version)
	assert.Equal(t, filepath.Join(dir, "url-shortener-infra-1.2.3.tgz"), sel.InfraTgz)
	assert.Equal(t, []chartTarball{{
		Name: "url-shortener-infra-jobs",
		Tgz:  filepath.Join(dir, "url-shortener-infra-jobs-1.2.3.tgz"),
	}}, sel.Extra)
}
