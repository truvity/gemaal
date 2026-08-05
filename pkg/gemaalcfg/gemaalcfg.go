// Package gemaalcfg loads the optional gemaal.yaml — the small committed
// file carrying a repo's deploy-and-test defaults: per-cluster helm
// values (riding installs as .Values.gemaal), repo-owned build hooks,
// the personal-namespace template, and the email→slug identity map (or
// a pointer to where a rendered map lives).
//
// Everything in it is overridable by flags and GEMAAL_* env; a missing
// file is not an error. It succeeds the fleet.yaml of the ocictl pilot,
// renamed and slimmed: the group-prefix machinery is gone — identity now
// flows through evidence drivers and resolution (pkg/identity), not
// through parsing the caller's cluster groups.
package gemaalcfg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// DefaultPath is where Load looks when no path is given.
const DefaultPath = "gemaal.yaml"

// DefaultPersonalNamespace is the conventional personal-namespace
// template; organizations override it in gemaal.yaml.
const DefaultPersonalNamespace = "emp-{slug}"

// slugPlaceholder must appear in every personal-namespace template.
const slugPlaceholder = "{slug}"

// slugShape is the RFC1123-label shape a slug must have — it gets
// embedded in a namespace name, so anything else would only fail later
// and further from the mistake.
var slugShape = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Config is the gemaal.yaml schema (v0 — may change until v0.1.0).
type Config struct {
	// PersonalNamespace is the template a standing install's namespace is
	// rendered from with the identity slug, e.g. "emp-{slug}".
	// DefaultPersonalNamespace when empty.
	PersonalNamespace string `yaml:"personalNamespace"`

	// Clusters maps KUBECONTEXT NAMES (kubeconfig stays the cluster
	// registry — no name indirection) to per-cluster deploy inputs.
	Clusters map[string]Cluster `yaml:"clusters"`

	// Hooks are opaque repo-owned commands — the repo-global fallback for
	// every project.
	Hooks Hooks `yaml:"hooks"`

	// Projects carries per-project overrides, keyed by project name.
	// Deliberately hooks-only: a monorepo builds each project its own
	// way, but this tool has no project model.
	Projects map[string]Project `yaml:"projects"`

	// Identity carries the email→slug map, or points at where it lives.
	Identity Identity `yaml:"identity"`

	// dir is the directory the config was loaded from; relative
	// identity.file paths resolve against it.
	dir string
}

// Cluster is one kubecontext's deploy inputs.
type Cluster struct {
	// Values is OPAQUE: merged into installs as .Values.gemaal, verbatim.
	// Organization semantics live in the charts, never in this tool.
	Values map[string]any `yaml:"values"`
}

// Project is one project's overrides. Hooks only, on purpose.
type Project struct {
	Hooks Hooks `yaml:"hooks"`
}

// Hooks are repo-owned commands this tool runs but never interprets.
type Hooks struct {
	// Build is an argv executed by `install --build` with the resolved
	// GEMAAL_* env exported.
	Build []string `yaml:"build"`
}

// Identity locates the email→slug map. Exactly one source: the inline
// map or the file, never both — two places to look is one place to be
// wrong.
type Identity struct {
	// Emails maps email → slug inline.
	Emails map[string]string `yaml:"emails"`

	// File points at a standalone YAML document holding that same flat
	// mapping (email: slug) — the shape a service-side config render
	// emits. Relative paths resolve against the config file's directory.
	File string `yaml:"file"`
}

// Load reads the file at path (or DefaultPath). A missing file returns
// an empty Config and no error — the file is optional by design.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Config{}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	cfg.dir = filepath.Dir(path)

	return cfg, nil
}

// Parse unmarshals (strictly — unknown fields are typos, not extensions)
// and validates a gemaal.yaml document.
func Parse(raw []byte) (*Config, error) {
	var cfg Config

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse gemaal.yaml: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	if c.PersonalNamespace != "" && !strings.Contains(c.PersonalNamespace, slugPlaceholder) {
		return fmt.Errorf("personalNamespace %q must contain %s", c.PersonalNamespace, slugPlaceholder)
	}

	if len(c.Identity.Emails) > 0 && c.Identity.File != "" {
		return errors.New("identity: emails and file are both set — the map must have exactly one source")
	}

	return validateEmailSlugs(c.Identity.Emails, "identity.emails")
}

// validateEmailSlugs asserts every entry of an email→slug map: keys are
// emails, values are namespace-embeddable slugs.
func validateEmailSlugs(m map[string]string, where string) error {
	for email, slug := range m {
		if _, _, ok := strings.Cut(email, "@"); !ok || strings.HasPrefix(email, "@") || strings.HasSuffix(email, "@") {
			return fmt.Errorf("%s: key %q is not an email", where, email)
		}

		if !slugShape.MatchString(slug) {
			return fmt.Errorf(
				"%s: slug %q (for %s) is not a valid RFC1123 label (lowercase alphanumerics and -, starting and ending alphanumeric)",
				where, slug, email)
		}
	}

	return nil
}

// EmailSlugs returns the identity map: the inline identity.emails, or
// the document identity.file points at. Nil (and no error) when the
// config carries neither — resolution then needs another source.
func (c *Config) EmailSlugs() (map[string]string, error) {
	if len(c.Identity.Emails) > 0 {
		return c.Identity.Emails, nil
	}

	if c.Identity.File == "" {
		return nil, nil
	}

	path := c.Identity.File
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.dir, path)
	}

	// A pointed-at file that is missing IS an error — unlike gemaal.yaml
	// itself, this one was explicitly promised.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("identity.file: %w", err)
	}

	var m map[string]string

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(&m); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse identity map %s: %w", path, err)
	}

	if err := validateEmailSlugs(m, path); err != nil {
		return nil, err
	}

	return m, nil
}

// BuildHook returns the effective build hook for a project: the
// project's own hooks.build when it sets one, otherwise the repo-global
// hooks.build. An empty (or unknown) project name asks for the global
// hook.
func (c *Config) BuildHook(project string) []string {
	if p, ok := c.Projects[project]; ok && len(p.Hooks.Build) > 0 {
		return p.Hooks.Build
	}

	return c.Hooks.Build
}

// ClusterValues returns the opaque values for one kubecontext, nil when
// the config has none.
func (c *Config) ClusterValues(kubecontext string) map[string]any {
	return c.Clusters[kubecontext].Values
}
