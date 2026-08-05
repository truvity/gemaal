package harness

import "strings"

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
