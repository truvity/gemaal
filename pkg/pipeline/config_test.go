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
