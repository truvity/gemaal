// Package config loads and validates the gemaal service configuration.
//
// Validation carries two of the design's safety rails (docs/design.md):
// parameter-store roots must be test-shaped (under /test/), and anything
// touching /secrets is refused outright. A configuration that fails either
// rule does not load, so the service never starts pointed at something
// that is not a test estate.
package config

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Defaults applied when the document omits a field.
const (
	DefaultListen               = ":8080"
	DefaultLabelDomain          = "gemaal.io"
	DefaultHousekeepingInterval = time.Minute
	DefaultGrace                = 48 * time.Hour
)

// testRoot is the only prefix a parameter-store root may live under: the
// non-test-shaped-config tripwire.
const testRoot = "/test/"

// Duration is a time.Duration that unmarshals from YAML strings like "24h".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var raw string
	if err := value.Decode(&raw); err != nil {
		return err
	}

	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", raw, err)
	}

	*d = Duration(parsed)

	return nil
}

// Std returns the standard-library form.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Tier holds the per-tier defaults a tenant inherits when its own labels
// are silent. Tiers differ only in numbers — one mechanism.
type Tier struct {
	// TTL is the tier's default time-to-live from last activity.
	TTL Duration `yaml:"ttl"`
}

// AllowList names the only places sweeps may ever touch: roots, not
// patterns.
type AllowList struct {
	S3Buckets []string `yaml:"s3Buckets"`
	SSMRoots  []string `yaml:"ssmRoots"`
}

// Config is the one typed document the service runs from.
type Config struct {
	Listen string `yaml:"listen"`
	// LabelDomain prefixes the ledger labels: <domain>/ttl,
	// <domain>/keep-until, <domain>/execution-id.
	LabelDomain string `yaml:"labelDomain"`
	// DryRun defaults to TRUE: a fresh deployment plans and reports,
	// executing deletions is a deliberate flip. Pointer so that absence
	// is distinguishable from an explicit false.
	DryRun               *bool           `yaml:"dryRun"`
	HousekeepingInterval Duration        `yaml:"housekeepingInterval"`
	Grace                Duration        `yaml:"grace"`
	Tiers                map[string]Tier `yaml:"tiers"`
	AllowList            AllowList       `yaml:"allowList"`
}

// Load reads, defaults, and validates a configuration document.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	return Parse(raw)
}

// Parse unmarshals, defaults, and validates a configuration document.
func Parse(raw []byte) (*Config, error) {
	var cfg Config

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}

	if c.LabelDomain == "" {
		c.LabelDomain = DefaultLabelDomain
	}

	if c.DryRun == nil {
		t := true
		c.DryRun = &t
	}

	if c.HousekeepingInterval == 0 {
		c.HousekeepingInterval = Duration(DefaultHousekeepingInterval)
	}

	if c.Grace == 0 {
		c.Grace = Duration(DefaultGrace)
	}
}

func (c *Config) validate() error {
	if strings.Trim(c.LabelDomain, ".") != c.LabelDomain || c.LabelDomain == "" {
		return fmt.Errorf("labelDomain %q: must be a bare domain without leading/trailing dots", c.LabelDomain)
	}

	for name, tier := range c.Tiers {
		if tier.TTL <= 0 {
			return fmt.Errorf("tier %q: ttl must be a positive duration", name)
		}
	}

	for _, root := range c.AllowList.SSMRoots {
		// Refused outright, before any shape check: the secrets tree has a
		// different writer and a different lifecycle. Never gemaal's.
		if strings.Contains(root, "/secrets") {
			return fmt.Errorf("ssm root %q: /secrets is never sweepable (refused structurally)", root)
		}

		// The non-test-shaped-config tripwire: a sweeper pointed anywhere
		// that does not look like a test estate is a deployment error.
		if !strings.HasPrefix(root, testRoot) {
			return fmt.Errorf("ssm root %q: not test-shaped — every root must live under %s", root, testRoot)
		}
	}

	return nil
}

// Labels returns the fully qualified ledger label keys for this
// configuration's domain.
func (c *Config) Labels() (ttl, keepUntil, executionID string) {
	return c.LabelDomain + "/ttl",
		c.LabelDomain + "/keep-until",
		c.LabelDomain + "/execution-id"
}

// TierTTL returns the default TTL for a tier and whether the tier is known.
func (c *Config) TierTTL(tier string) (time.Duration, bool) {
	t, ok := c.Tiers[tier]

	return t.TTL.Std(), ok
}
