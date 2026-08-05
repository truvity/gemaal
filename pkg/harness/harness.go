// Package harness resolves the STANDING tenant a test run belongs to,
// brackets the run with a suite's real phases (build, deploy, readiness,
// teardown — see Run and Suite), and gives tests the cluster-side
// helpers they need inside it: client-side helm installs stamped with
// the gemaal ledger labels, ring-pair ordering, rollout waits and
// Service ClusterIP resolution.
//
// Standing only, on purpose. The harness creates and deletes NOTHING
// itself — no namespace lifecycle, and the only teardown is the suite's
// own optional hooks over its own releases (interim, until the gemaal
// service's TTL housekeeping owns cleanup). An engineer's tenant is
// their personal namespace with the app's release; a CI run's tenant is
// a per-repo namespace with a per-run release. Cleanup belongs to the
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

	// Resolver overrides email→slug resolution outright (the seam a
	// future service-backed Resolve client plugs into). When nil, the
	// ladder in SlugResolver applies: the identity map from Config, else
	// the interim kubectl-groups resolver.
	Resolver identity.Resolver

	// IdentityRunner executes kubectl for the interim groups resolver
	// when the resolver ladder falls through to it (identity.ExecRunner
	// when nil) — the injection seam CLI tests script kubectl through.
	// Callers that set Resolver or a Config identity map never reach it.
	IdentityRunner identity.Runner
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

	resolver, err := o.SlugResolver()
	if err != nil {
		return "", err
	}

	slug, err := resolver.Resolve(ctx, res.Email)
	if err != nil {
		return "", fmt.Errorf("map %s (evidence: %s) to a slug: %w", res.Email, res.Driver, err)
	}

	return RenderNamespace(o.personalNamespace(), slug)
}

// SlugResolver returns the email→slug resolver the namespace ladder
// uses, in override order: Options.Resolver, else the gemaal.yaml
// identity map from Config, else the interim kubectl-groups resolver
// (identity.KubectlGroupsResolver — the emp:{slug} group in the
// caller's own cluster token; the sanctioned stand-in until the gemaal
// service's Resolve RPC, which stays the end state). Exported so
// gemaalctl whoami prints the slug through the exact resolver install
// uses — one path, no drift.
func (o Options) SlugResolver() (identity.Resolver, error) {
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

	return identity.KubectlGroupsResolver{
		Kubecontext: o.Kubecontext,
		Kubeconfig:  o.Kubeconfig,
		Runner:      o.IdentityRunner,
	}, nil
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
