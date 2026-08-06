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

	ttl, ok = cfg.TierTTL("employee")
	require.True(t, ok)
	assert.Equal(t, 14*time.Hour, ttl)

	assert.Equal(t, []string{"ci", "employee"}, cfg.TierValues())
	assert.Equal(t, "emp-jdoe", cfg.PersonalNamespace("jdoe"))
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

func TestNewDefaults(t *testing.T) {
	cfg, err := config.Parse([]byte("{}"))
	require.NoError(t, err)

	assert.Equal(t, config.DefaultTierLabel, cfg.TierLabel)
	assert.Equal(t, config.DefaultPersonalNamespace, cfg.Identity.PersonalNamespace)
	assert.Equal(t, config.DefaultGroupPrefix, cfg.Authz.GroupPrefix)
	assert.Equal(t, config.DefaultGroupsClaim, cfg.Authz.GroupsClaim)
	assert.Equal(t, config.DefaultHoldDefault, cfg.Hold.Default.Std())
	assert.Equal(t, config.DefaultHoldMax, cfg.Hold.Max.Std())
	assert.Empty(t, cfg.TierValues(), "no tiers configured means nothing is watched")
}

func TestNonTestShapedBucketRefused(t *testing.T) {
	_, err := config.Parse([]byte(`
allowList:
  s3Buckets:
    - truvity-prod-data
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not test-shaped")
}

func TestTestShapedBucketAccepted(t *testing.T) {
	_, err := config.Parse([]byte(`
allowList:
  s3Buckets:
    - truvity-devel-test
`))
	require.NoError(t, err)
}

func TestPersonalNamespaceNeedsSlug(t *testing.T) {
	_, err := config.Parse([]byte(`
identity:
  personalNamespace: emp-static
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "{slug}")
}

func TestIdentityMapValidated(t *testing.T) {
	_, err := config.Parse([]byte(`
identity:
  emails:
    not-an-email: jdoe
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an email")

	_, err = config.Parse([]byte(`
identity:
  emails:
    j.doe@example.com: "Not A Slug"
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RFC1123")
}

func TestHoldBoundsValidated(t *testing.T) {
	_, err := config.Parse([]byte(`
hold:
  default: 48h
  max: 8h
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds max")
}
