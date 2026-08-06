package harness

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Tenant is the tenant identity: the (namespace, release) pair one
// installation is named by. Everything else a test install names — state
// prefixes, databases, tagged objects — derives from this pair, so the
// two halves are resolved together and never independently.
type Tenant struct {
	// Namespace is the Kubernetes namespace the tenant lives in.
	Namespace string

	// Release is the Helm release name inside that namespace.
	Release string
}

// String renders the tenant as namespace/release.
func (t Tenant) String() string { return t.Namespace + "/" + t.Release }

// Environment variables of the GEMAAL_* contract: read as overrides,
// always re-exported with the RESOLVED values (bidirectional).
const (
	// EnvNamespace overrides namespace resolution.
	EnvNamespace = "GEMAAL_NAMESPACE"

	// EnvRelease overrides release resolution.
	EnvRelease = "GEMAAL_RELEASE"

	// EnvKubecontext carries the kubeconfig context the tenant was
	// resolved against.
	EnvKubecontext = "GEMAAL_KUBECONTEXT"

	// EnvServer overrides the gemaal service base URL the resolver
	// ladder's RPC rung calls (gemaal.yaml's server otherwise).
	EnvServer = "GEMAAL_SERVER"
)

// CI environment read for the CI release name. Present by construction
// on a GitHub Actions runner and absent everywhere else, which is what
// makes "am I CI?" a fact rather than a flag.
const (
	// EnvCIRunNumber is GitHub's per-repo-unique run counter.
	EnvCIRunNumber = "GITHUB_RUN_NUMBER"

	// EnvCIRunAttempt is the re-run attempt within one run.
	EnvCIRunAttempt = "GITHUB_RUN_ATTEMPT"
)

// Bounds asserted at resolution time rather than discovered in
// production: Helm caps release names, Kubernetes caps namespace labels.
const (
	// MaxNamespaceLen is the Kubernetes namespace-name bound.
	MaxNamespaceLen = 63

	// MaxReleaseLen is Helm's release-name bound. A ring-pair install
	// also asserts it for the derived "<release>-infra" companion.
	MaxReleaseLen = 53
)

// Env returns the tenant as KEY=VALUE pairs for a child process.
func (t Tenant) Env(kubecontext string) []string {
	env := []string{}

	if t.Namespace != "" {
		env = append(env, EnvNamespace+"="+t.Namespace)
	}

	if t.Release != "" {
		env = append(env, EnvRelease+"="+t.Release)
	}

	if kubecontext != "" {
		env = append(env, EnvKubecontext+"="+kubecontext)
	}

	return env
}

// Export sets Env in this process, so in-process consumers (and anything
// they spawn without an explicit Env) see the resolved tenant.
func (t Tenant) Export(kubecontext string) error {
	for _, kv := range t.Env(kubecontext) {
		k, v, _ := strings.Cut(kv, "=")

		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("export %s: %w", k, err)
		}
	}

	return nil
}

// rfc1123Label is the name shape Kubernetes and Helm both require.
var rfc1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// validName asserts the bound and the RFC1123 label shape both halves of
// the tenant identity must satisfy — failing here, with the offending
// name in hand, beats failing inside helm or the API server.
func validName(name, kind string, maxLen int) error {
	if name == "" {
		return fmt.Errorf("empty %s", kind)
	}

	if len(name) > maxLen {
		return fmt.Errorf("%s %q is %d characters, over the %d-character bound", kind, name, len(name), maxLen)
	}

	if !rfc1123Label.MatchString(name) {
		return fmt.Errorf(
			"%s %q is not a valid RFC1123 label (lowercase alphanumerics and -, starting and ending alphanumeric)",
			kind, name)
	}

	return nil
}
