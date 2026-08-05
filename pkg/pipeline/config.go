package pipeline

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/truvity/gemaal/pkg/config"
)

// Stamp values written to the .release-type provenance file by the two
// builds and verified by whichever flow pushes the charts. There are
// exactly two destinations, and each has exactly one entrypoint — the
// former RELEASE_TYPE env knob is gone on purpose: a shared entrypoint
// with a knob let artifacts built under one set of invariants be
// published under the other.
const (
	StampPreview = "preview"
	StampStable  = "stable"
)

// stampFileName is the provenance stamp written next to the packaged
// charts: which registry the chart values point at. The registry is both
// a build-time input and a push-time destination — `helmctl package
// --manifest --require-image-digests` bakes fully-qualified,
// digest-pinned image references (registry included) into the ring3
// chart at package time — so a push flow must be able to refuse charts
// packaged for the other registry.
const stampFileName = ".release-type"

// DefaultLockTimeout mirrors the shell scripts' CHART_LOCK_WAIT default:
// enough for the pure chart-output work with plenty of slack, short
// enough that a wedged holder surfaces as an error instead of a silent
// hang. A writer's window also spans its goreleaser run, so a contender
// arriving mid-release on a slow build can hit this legitimately.
const DefaultLockTimeout = 5 * time.Minute

// Destination is one registry + the AWS profile that may push to it.
type Destination struct {
	Registry string `yaml:"registry"`
	Profile  string `yaml:"profile"`
}

// Registries holds the exactly-two destinations. Each flow hard-picks
// its pair; nothing selects a destination at run time.
type Registries struct {
	Preview Destination `yaml:"preview"`
	Stable  Destination `yaml:"stable"`
}

// Chart names one ring chart: its packaged name and its source path
// (repo-relative).
type Chart struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
	// VendorDependencies runs `helm dependency update` before packaging
	// (file:// subcharts must be vendored; the helmctl CLI has no
	// --vendor-dependencies flag yet).
	VendorDependencies bool `yaml:"vendorDependencies"`
}

// Charts holds the ring pair. Ring2 is the infra chart (inert on its
// own), ring3 the app chart — the commit point that makes a version look
// released.
type Charts struct {
	Ring2 Chart `yaml:"ring2"`
	Ring3 Chart `yaml:"ring3"`
	// RepositoryPrefix prefixes the OCI repository each chart pushes to:
	// <prefix>/<chart name>. Default: <project>/charts.
	RepositoryPrefix string `yaml:"repositoryPrefix"`
}

// Commands holds the external tools as argv vectors — the same
// abstraction boundary the shell scripts had. helmctl defaults to
// `go tool helmctl`, matching the scripts' shell function.
type Commands struct {
	Goreleaser []string `yaml:"goreleaser"`
	Helm       []string `yaml:"helm"`
	Helmctl    []string `yaml:"helmctl"`
	AWS        []string `yaml:"aws"`
	// PreBuild commands run by release-stable ONLY, after the five gates
	// pass and before goreleaser (gates that run after a build are not
	// gates). The snapshot flow runs none: its caller's task deps are
	// expected to have produced any build inputs already.
	PreBuild [][]string `yaml:"preBuild"`
}

// Lock configures the one flock serializing every writer and reader of
// the packaged charts directory.
type Lock struct {
	// File is the lockfile path (repo-relative). THE INVARIANT: it lives
	// outside every cleaned path — flock is inode-based, and a lockfile
	// deleted mid-hold excludes nobody. `goreleaser --clean` wipes the
	// dist dir wholesale, so the lock sits NEXT TO it, never inside.
	File    string          `yaml:"file"`
	Timeout config.Duration `yaml:"timeout"`
}

// Hints are the rebuild commands quoted in error messages — whatever
// wraps gemaalctl in the consuming repo (moon tasks, just recipes, or
// gemaalctl itself).
type Hints struct {
	Snapshot string `yaml:"snapshot"`
	Stable   string `yaml:"stable"`
}

// Config is one project's pipeline configuration. Bar's url-shortener is
// one instance of it (see pipeline.example.yaml); nothing in the flows
// is hardcoded to any project.
type Config struct {
	// Project names the release unit: the goreleaser monorepo prefix, the
	// image repo path (<registry>/<project>) and most defaults below.
	Project string `yaml:"project"`
	// ProjectDir is where the project lives in the repo. Default: Project.
	ProjectDir string `yaml:"projectDir"`
	// DistDir is goreleaser's dist for the project. Default: dist/<project>.
	DistDir string `yaml:"distDir"`
	// GoreleaserConfig is the -f argument. Default: <projectDir>/.goreleaser.yaml.
	GoreleaserConfig string `yaml:"goreleaserConfig"`
	// TagPrefix scopes release tags: <tagPrefix>v*. Default: <project>/.
	TagPrefix string `yaml:"tagPrefix"`
	// ReleaseBranch is the only branch stable releases cut from. Default: master.
	ReleaseBranch string `yaml:"releaseBranch"`

	AWSRegion  string     `yaml:"awsRegion"`
	Registries Registries `yaml:"registries"`
	Charts     Charts     `yaml:"charts"`
	Commands   Commands   `yaml:"commands"`
	Lock       Lock       `yaml:"lock"`
	Hints      Hints      `yaml:"hints"`
}

// Load reads, defaults, and validates a pipeline configuration document.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pipeline config: %w", err)
	}

	return Parse(raw)
}

// Parse unmarshals, defaults, and validates a pipeline configuration
// document.
func Parse(raw []byte) (*Config, error) {
	var cfg Config

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse pipeline config: %w", err)
	}

	cfg.applyDefaults()

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.ProjectDir == "" {
		c.ProjectDir = c.Project
	}

	if c.DistDir == "" {
		c.DistDir = "dist/" + c.Project
	}

	if c.GoreleaserConfig == "" {
		c.GoreleaserConfig = c.ProjectDir + "/.goreleaser.yaml"
	}

	if c.TagPrefix == "" {
		c.TagPrefix = c.Project + "/"
	}

	if c.ReleaseBranch == "" {
		c.ReleaseBranch = "master"
	}

	if c.Charts.RepositoryPrefix == "" {
		c.Charts.RepositoryPrefix = c.Project + "/charts"
	}

	if len(c.Commands.Goreleaser) == 0 {
		c.Commands.Goreleaser = []string{"goreleaser"}
	}

	if len(c.Commands.Helm) == 0 {
		c.Commands.Helm = []string{"helm"}
	}

	if len(c.Commands.Helmctl) == 0 {
		c.Commands.Helmctl = []string{"go", "tool", "helmctl"}
	}

	if len(c.Commands.AWS) == 0 {
		c.Commands.AWS = []string{"aws"}
	}

	if c.Lock.File == "" {
		c.Lock.File = "dist/." + c.Project + "-charts.lock"
	}

	if c.Lock.Timeout == 0 {
		c.Lock.Timeout = config.Duration(DefaultLockTimeout)
	}

	if c.Hints.Snapshot == "" {
		c.Hints.Snapshot = "gemaalctl pipeline snapshot"
	}

	if c.Hints.Stable == "" {
		c.Hints.Stable = "gemaalctl pipeline release-stable"
	}
}

func (c *Config) validate() error {
	if c.Project == "" {
		return fmt.Errorf("pipeline config: project is required")
	}

	if c.AWSRegion == "" {
		return fmt.Errorf("pipeline config: awsRegion is required")
	}

	for name, dest := range map[string]Destination{
		"preview": c.Registries.Preview,
		"stable":  c.Registries.Stable,
	} {
		if dest.Registry == "" || dest.Profile == "" {
			return fmt.Errorf("pipeline config: registries.%s needs both registry and profile", name)
		}
	}

	for name, ch := range map[string]Chart{"ring2": c.Charts.Ring2, "ring3": c.Charts.Ring3} {
		if ch.Name == "" || ch.Path == "" {
			return fmt.Errorf("pipeline config: charts.%s needs both name and path", name)
		}
	}

	if c.Charts.Ring2.Name == c.Charts.Ring3.Name {
		return fmt.Errorf("pipeline config: ring2 and ring3 must have distinct chart names (both are %q)", c.Charts.Ring2.Name)
	}

	// THE LOCK INVARIANT: flock is inode-based, so a lockfile inside any
	// cleaned path (goreleaser --clean wipes DistDir wholesale) would be
	// deleted mid-hold and exclude nobody — the next acquirer locks a
	// fresh inode instantly. Refuse the configuration outright.
	lock := path.Clean(c.Lock.File)
	dist := path.Clean(c.DistDir)

	if lock == dist || strings.HasPrefix(lock, dist+"/") {
		return fmt.Errorf(
			"pipeline config: lock.file %q lives inside distDir %q — goreleaser --clean would delete the lock's inode mid-hold; place it outside every cleaned path",
			c.Lock.File, c.DistDir)
	}

	return nil
}

// ChartsOut is the packaged-charts output directory (repo-relative).
func (c *Config) ChartsOut() string { return path.Join(c.DistDir, "charts") }

// StampPath is the .release-type provenance stamp path (repo-relative).
func (c *Config) StampPath() string { return path.Join(c.ChartsOut(), stampFileName) }

// ManifestPath is the digest-pinned release manifest path (repo-relative).
func (c *Config) ManifestPath() string { return path.Join(c.DistDir, "chart-manifest.yaml") }

// ChartRepository is the OCI repository a chart pushes to.
func (c *Config) ChartRepository(name string) string { return c.Charts.RepositoryPrefix + "/" + name }
