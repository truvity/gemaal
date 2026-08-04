package config_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/gemaal/pkg/config"
)

func TestLoadExample(t *testing.T) {
	// The example document in the repo root must always load: it is the
	// first thing a stranger runs.
	cfg, err := config.Load(filepath.Join("..", "..", "config.example.yaml"))
	require.NoError(t, err)

	assert.Equal(t, ":8080", cfg.Listen)
	assert.Equal(t, "gemaal.io", cfg.LabelDomain)
	require.NotNil(t, cfg.DryRun)
	assert.True(t, *cfg.DryRun, "the example must keep the dry-run default")
	assert.Equal(t, time.Minute, cfg.HousekeepingInterval.Std())
	assert.Equal(t, 48*time.Hour, cfg.Grace.Std())

	ttl, ok := cfg.TierTTL("ci")
	require.True(t, ok)
	assert.Equal(t, 24*time.Hour, ttl)

	ttl, ok = cfg.TierTTL("emp")
	require.True(t, ok)
	assert.Equal(t, 14*time.Hour, ttl)
}

func TestDefaults(t *testing.T) {
	cfg, err := config.Parse([]byte("{}"))
	require.NoError(t, err)

	assert.Equal(t, config.DefaultListen, cfg.Listen)
	assert.Equal(t, config.DefaultLabelDomain, cfg.LabelDomain)
	require.NotNil(t, cfg.DryRun)
	assert.True(t, *cfg.DryRun, "dry-run must default to TRUE — executing is the deliberate flip")
	assert.Equal(t, config.DefaultHousekeepingInterval, cfg.HousekeepingInterval.Std())
	assert.Equal(t, config.DefaultGrace, cfg.Grace.Std())
}

func TestLabels(t *testing.T) {
	cfg, err := config.Parse([]byte("{}"))
	require.NoError(t, err)

	ttl, keepUntil, executionID := cfg.Labels()
	assert.Equal(t, "gemaal.io/ttl", ttl)
	assert.Equal(t, "gemaal.io/keep-until", keepUntil)
	assert.Equal(t, "gemaal.io/execution-id", executionID)
}

func TestLabelDomainConfigurable(t *testing.T) {
	cfg, err := config.Parse([]byte("labelDomain: gemaal.example.com"))
	require.NoError(t, err)

	ttl, _, _ := cfg.Labels()
	assert.Equal(t, "gemaal.example.com/ttl", ttl)
}

func TestSecretsRootRefused(t *testing.T) {
	_, err := config.Parse([]byte(`
allowList:
  ssmRoots:
    - /secrets/prod/
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/secrets")
}

// Even a secrets path smuggled under the test root is refused — the
// /secrets rule is checked before, and independently of, the shape rule.
func TestSecretsUnderTestRootRefused(t *testing.T) {
	_, err := config.Parse([]byte(`
allowList:
  ssmRoots:
    - /test/secrets/
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/secrets")
}

func TestNonTestShapedRootRefused(t *testing.T) {
	_, err := config.Parse([]byte(`
allowList:
  ssmRoots:
    - /roster/
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not test-shaped")
}

func TestUnknownFieldRefused(t *testing.T) {
	// KnownFields: a typo in the document is a startup error, not a
	// silently ignored knob.
	_, err := config.Parse([]byte("labelDomian: oops.io"))
	require.Error(t, err)
}

func TestNonPositiveTierTTLRefused(t *testing.T) {
	_, err := config.Parse([]byte(`
tiers:
  ci: {}
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ttl")
}
