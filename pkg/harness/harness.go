// Package harness resolves the STANDING tenant a test run belongs to
// and gives tests the cluster-side helpers they need inside it:
// client-side helm installs stamped with the gemaal ledger labels,
// ring-pair ordering, rollout waits and Service ClusterIP resolution.
//
// Standing only, on purpose. The harness creates and deletes NOTHING —
// no namespace lifecycle, no teardown. An engineer's tenant is their
// personal namespace with the app's release; a CI run's tenant is a
// per-repo namespace with a per-run release. Cleanup belongs to the
// gemaal service (TTL from last activity, keep-until, sweeps); the
// ephemeral create-run-destroy mode of this harness's predecessor was
// deliberately not ported.
//
// Resolution is one path shared by tests, gemaalctl and anything else,
// so their answers cannot drift:
//
//	namespace: Options.Namespace → GEMAAL_NAMESPACE →
//	           identity chain → email → slug → personal-namespace template
//	release:   Options.Release → GEMAAL_RELEASE →
//	           CI (r{run}-a{attempt}) → Options.App
package harness

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/truvity/gemaal/pkg/gemaalcfg"
	"github.com/truvity/gemaal/pkg/identity"
)

// Options configure resolution. The zero value resolves entirely from
// the environment — env overrides, then the default identity chain —
// though most callers set at least App and Config.
type Options struct {
	// Kubecontext targets a kubeconfig context (empty = current).
	Kubecontext string

	// Kubeconfig overrides the ambient kubeconfig for spawned tools.
	Kubeconfig string

	// Namespace overrides namespace resolution entirely (the
	// GEMAAL_NAMESPACE path).
	Namespace string

	// Release overrides release resolution entirely (the GEMAAL_RELEASE
	// path).
	Release string

	// App is the application name — the release of a standing employee
	// install, and the last rung of the release ladder.
	App string

	// PersonalNamespace is the identity-derived namespace template.
	// Wins over Config's; gemaalcfg.DefaultPersonalNamespace when both
	// are empty.
	PersonalNamespace string

	// Config supplies committed defaults (personal-namespace template,
	// identity map). Optional.
	Config *gemaalcfg.Config

	// Chain overrides the identity evidence chain
	// (identity.DefaultChain when empty). The seam for callers that
	// already resolved the email — identity.Chain{identity.Static(email)}
	// — and for tests.
	Chain identity.Chain

	// Resolver overrides email→slug resolution (the seam a future
	// service-backed Resolve client plugs into). When nil, the identity
	// map from Config serves.
	Resolver identity.Resolver
}

// Resolve derives the tenant and touches nothing: no cluster call unless
// the identity rung is reached, no env written (see Tenant.Export). Both
// halves are bounds- and shape-asserted before anything uses them.
func Resolve(ctx context.Context, o Options) (Tenant, error) {
	namespace, err := ResolveNamespace(ctx, o)
	if err != nil {
		return Tenant{}, err
	}

	release, err := ResolveRelease(o)
	if err != nil {
		return Tenant{}, err
	}

	return Tenant{Namespace: namespace, Release: release}, nil
}

// ResolveRelease resolves the release half of the tenant, for callers
// that need the halves independently (whoami prints each on its own — a
// missing release must not hide the namespace). Resolve calls this one.
func ResolveRelease(o Options) (string, error) {
	release, err := resolveRelease(o)
	if err != nil {
		return "", err
	}

	if err := validName(release, "release", MaxReleaseLen); err != nil {
		return "", err
	}

	return release, nil
}

// ResolveNamespace resolves the namespace half of the tenant, for
// callers that need only that (whoami's independent halves, a bare
// `kubectl -n`). Resolve is the full path and calls this one.
func ResolveNamespace(ctx context.Context, o Options) (string, error) {
	namespace, err := resolveNamespace(ctx, o)
	if err != nil {
		return "", err
	}

	if err := validName(namespace, "namespace", MaxNamespaceLen); err != nil {
		return "", err
	}

	return namespace, nil
}

func resolveNamespace(ctx context.Context, o Options) (string, error) {
	if o.Namespace != "" {
		return o.Namespace, nil
	}

	if ns := os.Getenv(EnvNamespace); ns != "" {
		return ns, nil
	}

	// The identity rung: evidence → email → slug → template.
	chain := o.Chain
	if len(chain) == 0 {
		chain = identity.DefaultChain(o.Kubecontext, o.Kubeconfig)
	}

	res, err := chain.Resolve(ctx)
	if err != nil {
		return "", err
	}

	resolver, err := o.resolver()
	if err != nil {
		return "", err
	}

	slug, err := resolver.Resolve(ctx, res.Email)
	if err != nil {
		return "", fmt.Errorf("map %s (evidence: %s) to a slug: %w", res.Email, res.Driver, err)
	}

	return RenderNamespace(o.personalNamespace(), slug)
}

// resolver picks the slug source: the explicit override, else the
// config's identity map.
func (o Options) resolver() (identity.Resolver, error) {
	if o.Resolver != nil {
		return o.Resolver, nil
	}

	if o.Config != nil {
		m, err := o.Config.EmailSlugs()
		if err != nil {
			return nil, err
		}

		if m != nil {
			return identity.MapResolver(m), nil
		}
	}

	return nil, fmt.Errorf(
		"no slug resolver: set Options.Resolver, or give Options.Config a gemaal.yaml identity map (identity.emails or identity.file)")
}

func (o Options) personalNamespace() string {
	if o.PersonalNamespace != "" {
		return o.PersonalNamespace
	}

	if o.Config != nil && o.Config.PersonalNamespace != "" {
		return o.Config.PersonalNamespace
	}

	return gemaalcfg.DefaultPersonalNamespace
}

func resolveRelease(o Options) (string, error) {
	if o.Release != "" {
		return o.Release, nil
	}

	if r := os.Getenv(EnvRelease); r != "" {
		return r, nil
	}

	if r, ok := ciRelease(); ok {
		return r, nil
	}

	if o.App != "" {
		return o.App, nil
	}

	return "", fmt.Errorf(
		"no release: set Options.Release, %s, or Options.App (the standing install's app name); "+
			"CI derives r{run}-a{attempt} from %s/%s",
		EnvRelease, EnvCIRunNumber, EnvCIRunAttempt)
}

// ciRelease renders r{run_number}-a{attempt} when the CI environment is
// present. GITHUB_RUN_NUMBER is per-repo-unique by GitHub's semantics
// and the CI namespace already carries the repo, so the pair is unique.
func ciRelease() (string, bool) {
	run := strings.TrimSpace(os.Getenv(EnvCIRunNumber))
	if run == "" {
		return "", false
	}

	attempt := strings.TrimSpace(os.Getenv(EnvCIRunAttempt))
	if attempt == "" {
		attempt = "1"
	}

	return "r" + run + "-a" + attempt, true
}

// RenderNamespace renders the personal-namespace template with a slug.
// The only substitution is {slug} — kept deliberately dumb.
func RenderNamespace(template, slug string) (string, error) {
	if !strings.Contains(template, "{slug}") {
		return "", fmt.Errorf("personal-namespace template %q must contain {slug}", template)
	}

	return strings.ReplaceAll(template, "{slug}", slug), nil
}

// TestMain is the subset of *testing.M this package needs — an interface
// so the harness is testable without a real test binary.
type TestMain interface{ Run() int }

// Run is the TestMain entry point: resolve the standing tenant, export
// the GEMAAL_* contract, run the tests. Nothing is created and nothing
// is torn down — installs are the caller's own client-side helm (see
// Cluster), cleanup is the gemaal service's job.
//
//	func TestMain(m *testing.M) {
//	    cfg, err := gemaalcfg.Load("../gemaal.yaml")
//	    if err != nil { ... }
//	    os.Exit(harness.Run(m, harness.Options{
//	        Kubecontext: "devel@oidc",
//	        App:         "url-shortener",
//	        Config:      cfg,
//	    }))
//	}
func Run(m TestMain, o Options) int {
	tenant, err := Resolve(context.Background(), o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)

		return 1
	}

	if err := tenant.Export(o.Kubecontext); err != nil {
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)

		return 1
	}

	fmt.Fprintf(os.Stderr, "harness: standing tenant %s\n", tenant)

	return m.Run()
}
