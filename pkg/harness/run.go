package harness

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// The GEMAAL_TEST_* skip contract — the operator's dials on a suite run.
// Values are strconv.ParseBool booleans; a set-but-unparseable value is
// an error, never silently false (a misspelled "keep" that quietly
// destroys the releases it was told to keep is the worst possible
// reading).
const (
	// EnvSkipBuild skips the Build hook: use whatever artifacts are
	// already lying around (deploy still happens, so they must exist).
	EnvSkipBuild = "GEMAAL_TEST_SKIP_BUILD"

	// EnvSkipDeploy skips Build AND Deploy: reuse the standing releases
	// exactly as installed.
	EnvSkipDeploy = "GEMAAL_TEST_SKIP_DEPLOY"

	// EnvSkipDestroy skips the Teardown hooks: keep the releases after
	// the run.
	EnvSkipDestroy = "GEMAAL_TEST_SKIP_DESTROY"

	// EnvKeep is EnvSkipDestroy's spoken-form alias.
	EnvKeep = "GEMAAL_TEST_KEEP"
)

// DefaultTeardownTimeout bounds the Teardown phase as a whole. Generous
// on purpose: finalizer-heavy infrastructure charts tear down external
// resources.
const DefaultTeardownTimeout = 15 * time.Minute

// SkipFlags is the parsed GEMAAL_TEST_* contract, with the implications
// already applied: skipping deploy implies skipping the build that only
// feeds it.
type SkipFlags struct {
	// Build skips the Build hook (EnvSkipBuild, or implied by Deploy).
	Build bool

	// Deploy skips the Build and Deploy hooks (EnvSkipDeploy).
	Deploy bool

	// Destroy skips the Teardown hooks (EnvSkipDestroy or EnvKeep).
	Destroy bool
}

// ParseSkipFlags reads the GEMAAL_TEST_* environment. Unset and empty
// variables are false; set-but-unparseable values are errors.
func ParseSkipFlags() (SkipFlags, error) {
	var flags SkipFlags

	for _, v := range []struct {
		name string
		into *bool
	}{
		{EnvSkipBuild, &flags.Build},
		{EnvSkipDeploy, &flags.Deploy},
		{EnvSkipDestroy, &flags.Destroy},
		{EnvKeep, &flags.Destroy},
	} {
		raw := os.Getenv(v.name)
		if raw == "" {
			continue
		}

		value, err := strconv.ParseBool(raw)
		if err != nil {
			return SkipFlags{}, fmt.Errorf("%s=%q is not a boolean (want a strconv.ParseBool value like 1/true)", v.name, raw)
		}

		*v.into = *v.into || value
	}

	flags.Build = flags.Build || flags.Deploy

	return flags, nil
}

// TestMain is the subset of *testing.M this package needs — an interface
// so the harness is testable without a real test binary.
type TestMain interface{ Run() int }

// Hook is one step of a suite phase, handed the cluster toolbox and the
// resolved tenant.
type Hook func(ctx context.Context, cluster *Cluster, tenant Tenant) error

// Suite configures Run: tenant resolution (the embedded Options) plus
// the phases a real integration suite brackets m.Run() with. Every
// phase is optional — the zero Suite resolves, exports and runs, and a
// real one adds artifact builds, installs, rollout waits and URL
// resolution without re-implementing the skip-flag plumbing.
type Suite struct {
	Options

	// Cluster is the toolbox handed to every hook; derived from the
	// Options' kubecontext/kubeconfig when nil.
	Cluster *Cluster

	// Build produces the artifacts Deploy installs (typically the
	// gemaal.yaml build hook). Skipped by EnvSkipBuild and EnvSkipDeploy.
	Build Hook

	// Deploy installs into the standing tenant (typically
	// Cluster.InstallPair with ledger labels). Skipped by EnvSkipDeploy.
	Deploy Hook

	// Setup runs after Deploy, in order, never skipped: rollout waits,
	// URL resolution, test globals — a reused install (EnvSkipDeploy)
	// still has to be ready and reachable.
	Setup []Hook

	// Teardown runs after m.Run(), in order, with a fresh context — it
	// is registered BEFORE Build/Deploy, so a partially-installed suite
	// is torn down too. Skipped by EnvSkipDestroy/EnvKeep. Errors are
	// reported but do not change the exit code: what a failed local
	// teardown leaves behind is a standing tenant, and draining those is
	// the gemaal service's job. Deployments running the service's TTL
	// housekeeping can omit Teardown entirely — leaving the pair in
	// place is the intended end state.
	Teardown []Hook

	// TeardownTimeout bounds the whole Teardown phase;
	// DefaultTeardownTimeout when zero.
	TeardownTimeout time.Duration
}

// Run is the TestMain entry point: parse the skip flags, resolve the
// standing tenant, export the GEMAAL_* contract, then bracket m.Run()
// with the suite's phases — Build → Deploy → Setup before, Teardown
// after. The run context cancels on SIGINT/SIGTERM; Teardown gets its
// own context so a canceled run is still cleaned up. The harness itself
// still creates and deletes NOTHING — what the hooks install is the
// caller's own client-side helm, and cleanup beyond the suite's
// Teardown is the gemaal service's job.
//
//	func TestMain(m *testing.M) {
//	    flag.Parse()
//	    if testing.Short() { return } // unit-only run
//	    cfg, err := gemaalcfg.Load("gemaal.yaml")
//	    if err != nil { ... }
//	    os.Exit(harness.Run(m, harness.Suite{
//	        Options: harness.Options{Kubecontext: "devel@oidc", App: "url-shortener", Config: cfg},
//	        Build:   buildArtifacts,
//	        Deploy:  installRingPair,
//	        Setup:   []harness.Hook{waitAndResolveURLs},
//	        Teardown: []harness.Hook{uninstallRingPair}, // interim, until the service owns cleanup
//	    }))
//	}
func Run(m TestMain, s Suite) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code, err := s.run(ctx, m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)

		return 1
	}

	return code
}

func (s Suite) run(ctx context.Context, m TestMain) (int, error) {
	skip, err := ParseSkipFlags()
	if err != nil {
		return 1, err
	}

	tenant, err := Resolve(ctx, s.Options)
	if err != nil {
		return 1, err
	}

	if err := tenant.Export(s.Kubecontext); err != nil {
		return 1, err
	}

	fmt.Fprintf(os.Stderr, "harness: standing tenant %s\n", tenant)

	cluster := s.cluster()

	defer s.teardown(cluster, tenant, skip)

	if err := s.installPhases(ctx, cluster, tenant, skip); err != nil {
		return 1, err
	}

	for _, hook := range s.Setup {
		if err := hook(ctx, cluster, tenant); err != nil {
			return 1, fmt.Errorf("setup: %w", err)
		}
	}

	return m.Run(), nil
}

// installPhases runs Build then Deploy under the skip flags.
func (s Suite) installPhases(ctx context.Context, cluster *Cluster, tenant Tenant, skip SkipFlags) error {
	if skip.Deploy {
		fmt.Fprintf(os.Stderr, "harness: skipping build and deploy (%s)\n", EnvSkipDeploy)

		return nil
	}

	switch {
	case skip.Build:
		fmt.Fprintf(os.Stderr, "harness: skipping build (%s)\n", EnvSkipBuild)
	case s.Build != nil:
		if err := s.Build(ctx, cluster, tenant); err != nil {
			return fmt.Errorf("build: %w", err)
		}
	}

	if s.Deploy != nil {
		if err := s.Deploy(ctx, cluster, tenant); err != nil {
			return fmt.Errorf("deploy: %w", err)
		}
	}

	return nil
}

// teardown runs the Teardown hooks under a fresh bounded context — the
// run's own context may already be canceled, and cleanup must not
// depend on it.
func (s Suite) teardown(cluster *Cluster, tenant Tenant, skip SkipFlags) {
	if len(s.Teardown) == 0 {
		return
	}

	if skip.Destroy {
		fmt.Fprintf(os.Stderr, "harness: keeping %s (%s/%s)\n", tenant, EnvSkipDestroy, EnvKeep)

		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.teardownTimeout())
	defer cancel()

	for _, hook := range s.Teardown {
		if err := hook(ctx, cluster, tenant); err != nil {
			fmt.Fprintf(os.Stderr, "harness: teardown: %v\n", err)
		}
	}
}

func (s Suite) cluster() *Cluster {
	if s.Cluster != nil {
		return s.Cluster
	}

	return &Cluster{Kubecontext: s.Kubecontext, Kubeconfig: s.Kubeconfig}
}

func (s Suite) teardownTimeout() time.Duration {
	if s.TeardownTimeout > 0 {
		return s.TeardownTimeout
	}

	return DefaultTeardownTimeout
}
