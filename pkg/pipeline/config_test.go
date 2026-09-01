package pipeline

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDefaults(t *testing.T) {
	cfg := testConfig(t)

	assert.Equal(t, "url-shortener", cfg.ProjectDir)
	assert.Equal(t, "dist/url-shortener", cfg.DistDir)
	assert.Equal(t, "url-shortener/.goreleaser.yaml", cfg.GoreleaserConfig)
	assert.Equal(t, "url-shortener/", cfg.TagPrefix)
	assert.Equal(t, "master", cfg.ReleaseBranch)
	assert.Equal(t, "url-shortener/charts", cfg.Charts.RepositoryPrefix)
	assert.Equal(t, []string{"goreleaser"}, cfg.Commands.Goreleaser)
	assert.Equal(t, []string{"helm"}, cfg.Commands.Helm)
	assert.Equal(t, []string{"go", "tool", "helmctl"}, cfg.Commands.Helmctl)
	assert.Equal(t, []string{"aws"}, cfg.Commands.AWS)
	assert.Equal(t, "dist/.url-shortener-charts.lock", cfg.Lock.File)
	assert.Equal(t, 5*time.Minute, cfg.Lock.Timeout.Std())
	assert.Equal(t, "gemaalctl pipeline snapshot", cfg.Hints.Snapshot)
	assert.Equal(t, "gemaalctl pipeline release-stable", cfg.Hints.Stable)

	assert.Equal(t, "dist/url-shortener/charts", cfg.ChartsOut())
	assert.Equal(t, "dist/url-shortener/charts/.release-type", cfg.StampPath())
	assert.Equal(t, "dist/url-shortener/chart-manifest.yaml", cfg.ManifestPath())
	assert.Equal(t, "url-shortener/charts/url-shortener", cfg.ChartRepository("url-shortener"))
}

func TestParseValidation(t *testing.T) {
	base := func(mutate func(c *Config)) *Config {
		cfg := testConfig(t)
		mutate(cfg)

		return cfg
	}

	cases := []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{
			name:    "missing project",
			cfg:     base(func(c *Config) { c.Project = "" }),
			wantErr: "project is required",
		},
		{
			name:    "missing aws region",
			cfg:     base(func(c *Config) { c.AWSRegion = "" }),
			wantErr: "awsRegion is required",
		},
		{
			name:    "missing stable profile",
			cfg:     base(func(c *Config) { c.Registries.Stable.Profile = "" }),
			wantErr: "registries.stable",
		},
		{
			name:    "missing ring2 path",
			cfg:     base(func(c *Config) { c.Charts.Ring2.Path = "" }),
			wantErr: "charts.ring2",
		},
		{
			name:    "identical ring names",
			cfg:     base(func(c *Config) { c.Charts.Ring2.Name = c.Charts.Ring3.Name }),
			wantErr: "distinct chart names",
		},
		{
			name:    "extra chart without a path",
			cfg:     base(func(c *Config) { c.Charts.Extra = []Chart{{Name: "broker"}} }),
			wantErr: "charts.extra[0] needs both name and path",
		},
		{
			// A name collision is a SILENT overwrite: both charts are
			// packaged as <name>-<version>.tgz into one directory, and the
			// selector — which keys on filenames — would see a complete,
			// coherent set built from one tarball wearing two identities.
			name: "extra chart name collides with ring3",
			cfg: base(func(c *Config) {
				c.Charts.Extra = []Chart{{Name: c.Charts.Ring3.Name, Path: "charts/broker"}}
			}),
			wantErr: `name "url-shortener" is already charts.ring3`,
		},
		{
			name: "two extra charts share a name",
			cfg: base(func(c *Config) {
				c.Charts.Extra = []Chart{
					{Name: "broker", Path: "charts/broker"},
					{Name: "broker", Path: "charts/other"},
				}
			}),
			wantErr: `charts.extra[1] name "broker" is already charts.extra[0]`,
		},
		{
			// THE LOCK INVARIANT: goreleaser --clean wipes distDir, and a
			// lockfile whose inode is deleted mid-hold excludes nobody.
			name:    "lockfile inside distDir",
			cfg:     base(func(c *Config) { c.Lock.File = "dist/url-shortener/charts.lock" }),
			wantErr: "outside every cleaned path",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte("project: x\nreleaseType: preview\n"))
	require.Error(t, err)
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/pipeline.yaml")
	require.Error(t, err)
}

// TestExampleConfigLoads keeps pipeline.example.yaml honest: the
// documented url-shortener instance must always parse and validate.
func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load("../../pipeline.example.yaml")
	require.NoError(t, err)

	assert.Equal(t, "url-shortener", cfg.Project)
	assert.Equal(t, "url-shortener-infra", cfg.Charts.Ring2.Name)
	assert.True(t, cfg.Charts.Ring2.VendorDependencies)
	assert.Equal(t, []string{"go", "tool", "helmctl"}, cfg.Commands.Helmctl)
	require.Len(t, cfg.Commands.PreBuild, 1)
	assert.Equal(t, "dist/.url-shortener-charts.lock", cfg.Lock.File)
}

// TestParseAcceptsExtraCharts is the whole point: a repo whose one tag
// produces a chart that is neither ring must be able to say so — and the
// tarball scan must file that chart's tarball under IT, not under the
// ring3 name its own name happens to begin with.
func TestParseAcceptsExtraCharts(t *testing.T) {
	cfg, err := Parse([]byte(testExtraConfigYAML))
	require.NoError(t, err)

	name, version, ok := cfg.Charts.match("url-shortener-broker-1.2.3")
	require.True(t, ok)
	assert.Equal(t, "url-shortener-broker", name)
	assert.Equal(t, "1.2.3", version)
	assert.Equal(t, "url-shortener/charts/url-shortener-broker", cfg.ChartRepository(name))

	// The rings still resolve to themselves — ring2's longer name wins
	// over ring3's, which also prefixes its tarball.
	name, version, ok = cfg.Charts.match("url-shortener-infra-1.2.3")
	require.True(t, ok)
	assert.Equal(t, "url-shortener-infra", name)
	assert.Equal(t, "1.2.3", version)
}
