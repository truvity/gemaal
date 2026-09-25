package harness

import (
	"os"
	"strings"
)

// Client-side tier names, as derived from the namespace-name convention
// by TierForNamespace.
const (
	// TierEmployee is a standing personal install's tier (emp-{slug}).
	TierEmployee = "employee"

	// TierCI is a CI install's tier (ci-{org}-{repo}).
	TierCI = "ci"

	// TierUnknown is any namespace outside the convention.
	TierUnknown = "unknown"
)

// TierForNamespace derives the tier from the namespace-name convention:
// emp- → employee, ci- → ci, anything else → unknown. This is the
// CLIENT's helper — the one rule consumers kept re-implementing to
// stamp tier values into their charts. The service side never uses it:
// there, the tier LABEL on the namespace is the machine selector, and
// name prefixes stay a human convention (docs/design.md, "Tiers").
func TierForNamespace(namespace string) string {
	switch {
	case strings.HasPrefix(namespace, "emp-"):
		return TierEmployee
	case strings.HasPrefix(namespace, "ci-"):
		return TierCI
	default:
		return TierUnknown
	}
}

// EnvTier names the environment/context tier explicitly — set by CI so
// kind detection never depends on guessing a kube-context's name.
const EnvTier = "GEMAAL_TIER"

// TierKind is the disposable local/CI kind cluster: no service-CIDR
// route reaches it (a laptop's Docker Desktop cannot route to one at
// all), so (*Cluster).ServiceURL opens a port-forward instead of
// dialing the ClusterIP directly, and the project's own ring2 is never
// installed there — a fixture substitutes for it, which is what
// DeployApp (ring3 alone) exists for.
//
// This is a DIFFERENT AXIS from TierForNamespace's employee/ci/unknown:
// that one classifies a NAMESPACE NAME for charts that want it as a
// value; this one answers "which kind of cluster is this run talking
// to", which the harness itself acts on. A namespace can carry either
// kind of tier independently — nothing requires them to agree, and nothing
// here renames or replaces TierForNamespace.
const TierKind = "kind"

// DetectTier reports the environment/context tier for a kubecontext
// name: EnvTier when set (any value — TierKind is the only one the
// harness treats specially today), else TierKind when the context
// carries kind's own "kind-<cluster>" naming convention, else "" — the
// shared-cluster tier every existing caller already runs on, unchanged.
func DetectTier(kubecontext string) string {
	if t := strings.TrimSpace(os.Getenv(EnvTier)); t != "" {
		return t
	}

	if strings.HasPrefix(kubecontext, "kind-") {
		return TierKind
	}

	return ""
}
