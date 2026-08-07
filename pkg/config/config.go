// Package config loads and validates the gemaal service configuration.
//
// Validation carries the design's safety rails (docs/design.md):
// parameter-store roots must be test-shaped (under /test/), anything
// touching /secrets is refused outright, and S3 buckets must carry a
// "test" segment. A configuration that fails any rule does not load, so
// the service never starts pointed at something that is not a test
// estate.
package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"sort"
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
	DefaultTierLabel            = "tenancy.truvity.io/tier"
	DefaultPersonalNamespace    = "emp-{slug}"
	DefaultGroupPrefix          = "emp:"
	DefaultGroupsClaim          = "groups"
	DefaultHoldDefault          = 8 * time.Hour
	DefaultHoldMax              = 7 * 24 * time.Hour
	// DefaultAWSRegion is where the estate lives (the PIA policy and the
	// /test/ parameters are regional); see Config.AWSRegion for why the
	// region cannot come from the deployment environment.
	DefaultAWSRegion = "eu-central-1"
)

// testRoot is the only prefix a parameter-store root may live under: the
// non-test-shaped-config tripwire.
const testRoot = "/test/"

// slugPlaceholder must appear in the personal-namespace template.
const slugPlaceholder = "{slug}"

// testShapedBucket requires a "test" path segment in the bucket name,
// e.g. "truvity-devel-test". Per-install production-shaped buckets
// deliberately fail it (ported from the janitor prototype).
var testShapedBucket = regexp.MustCompile(`(^|-)test(-|$)`)

// slugShape is the RFC1123-label shape a slug must have — it gets
// embedded in a namespace name.
var slugShape = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

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

// Identity is the service-side resolution authority's data: the
// email→slug map (rendered by the deployer from the personnel source)
// and the template a slug's standing namespace derives from.
type Identity struct {
	// PersonalNamespace renders a slug's standing namespace;
	// DefaultPersonalNamespace when empty. {slug} is the only
	// substitution.
	PersonalNamespace string `yaml:"personalNamespace"`

	// Emails maps email → slug. The deployer renders it; the service
	// never invents identities.
	Emails map[string]string `yaml:"emails"`
}

// Authz carries the authorization knobs for the mutating RPCs.
type Authz struct {
	// AdminGroups may Sweep and hold any tenant. Empty means nobody is
	// an admin — Sweep then refuses everyone, which is the safe default.
	AdminGroups []string `yaml:"adminGroups"`

	// GroupPrefix marks the slug-bearing group in a caller's groups
	// claim ("emp:{slug}"); DefaultGroupPrefix when empty.
	GroupPrefix string `yaml:"groupPrefix"`

	// GroupsClaim names the OIDC claim carrying groups;
	// DefaultGroupsClaim when empty.
	GroupsClaim string `yaml:"groupsClaim"`

	// TokenAudiences are passed to TokenReview; empty means the API
	// server's default audience.
	TokenAudiences []string `yaml:"tokenAudiences"`
}

// Hold bounds the Checkout/Extend keep-until stamps.
type Hold struct {
	// Default is used when a request carries no duration.
	Default Duration `yaml:"default"`

	// Max clamps every requested duration.
	Max Duration `yaml:"max"`
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
	DryRun               *bool    `yaml:"dryRun"`
	HousekeepingInterval Duration `yaml:"housekeepingInterval"`
	Grace                Duration `yaml:"grace"`
	// Kubecontext targets a kubeconfig context; empty means the ambient
	// (in-cluster) configuration — the normal deployment.
	Kubecontext string `yaml:"kubecontext"`
	// TierLabel is the namespace label whose VALUE selects the tier;
	// DefaultTierLabel when empty. A namespace without it does not exist
	// as far as gemaal is concerned.
	TierLabel string          `yaml:"tierLabel"`
	Tiers     map[string]Tier `yaml:"tiers"`
	AllowList AllowList       `yaml:"allowList"`
	Identity  Identity        `yaml:"identity"`
	Authz     Authz           `yaml:"authz"`
	Hold      Hold            `yaml:"hold"`
	// AWSRegion pins the AWS SDK region. When the document omits it,
	// AWS_REGION from the environment applies, then DefaultAWSRegion.
	// Explicit-with-a-default because the deployment cannot lean on the
	// environment: EKS Pod Identity injects only the credential-endpoint
	// variables (AWS_CONTAINER_CREDENTIALS_FULL_URI and its token file),
	// never AWS_REGION, and IMDS is not reachable from pods — so an
	// unpinned region leaves the SSM client unable to resolve one at all.
	AWSRegion string `yaml:"awsRegion"`
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

	if c.TierLabel == "" {
		c.TierLabel = DefaultTierLabel
	}

	if c.Identity.PersonalNamespace == "" {
		c.Identity.PersonalNamespace = DefaultPersonalNamespace
	}

	if c.Authz.GroupPrefix == "" {
		c.Authz.GroupPrefix = DefaultGroupPrefix
	}

	if c.Authz.GroupsClaim == "" {
		c.Authz.GroupsClaim = DefaultGroupsClaim
	}

	if c.Hold.Default == 0 {
		c.Hold.Default = Duration(DefaultHoldDefault)
	}

	if c.Hold.Max == 0 {
		c.Hold.Max = Duration(DefaultHoldMax)
	}

	// Config beats environment beats default, and the resolution happens
	// HERE so the effective region is decided in one visible place — the
	// SDK's own environment fallback never engages.
	if c.AWSRegion == "" {
		c.AWSRegion = os.Getenv("AWS_REGION")
	}

	if c.AWSRegion == "" {
		c.AWSRegion = DefaultAWSRegion
	}
}

func (c *Config) validate() error {
	if strings.Trim(c.LabelDomain, ".") != c.LabelDomain || c.LabelDomain == "" {
		return fmt.Errorf("labelDomain %q: must be a bare domain without leading/trailing dots", c.LabelDomain)
	}

	if strings.TrimSpace(c.TierLabel) != c.TierLabel || c.TierLabel == "" {
		return fmt.Errorf("tierLabel %q: must be a non-empty label key", c.TierLabel)
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

	for _, bucket := range c.AllowList.S3Buckets {
		// Same tripwire in S3 form (ported from the janitor prototype):
		// a bucket without a "test" segment is not a test estate.
		if !testShapedBucket.MatchString(bucket) {
			return fmt.Errorf("s3 bucket %q: not test-shaped — the name needs a \"test\" segment", bucket)
		}
	}

	if !strings.Contains(c.Identity.PersonalNamespace, slugPlaceholder) {
		return fmt.Errorf("identity.personalNamespace %q must contain %s", c.Identity.PersonalNamespace, slugPlaceholder)
	}

	for email, slug := range c.Identity.Emails {
		if _, _, ok := strings.Cut(email, "@"); !ok || strings.HasPrefix(email, "@") || strings.HasSuffix(email, "@") {
			return fmt.Errorf("identity.emails: key %q is not an email", email)
		}

		if !slugShape.MatchString(slug) {
			return fmt.Errorf("identity.emails: slug %q (for %s) is not a valid RFC1123 label", slug, email)
		}
	}

	if c.Hold.Default <= 0 || c.Hold.Max <= 0 {
		return fmt.Errorf("hold: default and max must be positive durations")
	}

	if c.Hold.Default > c.Hold.Max {
		return fmt.Errorf("hold: default %s exceeds max %s", c.Hold.Default.Std(), c.Hold.Max.Std())
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

// TierValues returns the configured tier label values, sorted — the
// namespace selector's right-hand side. A tier absent here is a tier
// gemaal does not watch.
func (c *Config) TierValues() []string {
	values := make([]string, 0, len(c.Tiers))
	for name := range c.Tiers {
		values = append(values, name)
	}

	// Sorted for deterministic selectors (and deterministic tests).
	sort.Strings(values)

	return values
}

// PersonalNamespace renders the standing namespace for a slug.
func (c *Config) PersonalNamespace(slug string) string {
	return strings.ReplaceAll(c.Identity.PersonalNamespace, slugPlaceholder, slug)
}
