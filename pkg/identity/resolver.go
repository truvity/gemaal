package identity

import (
	"context"
	"fmt"
	"strings"
)

// Resolver maps an email to the slug the organization knows it by. The
// interface is the seam: MapResolver serves a committed map,
// ResolveClient asks a deployed gemaal service's Resolve RPC (the
// authority, rendered from the personnel source), and
// KubectlGroupsResolver is the sanctioned interim for deployments with
// neither — retiring in stages behind the RPC rung.
type Resolver interface {
	Resolve(ctx context.Context, email string) (slug string, err error)
}

// MapResolver resolves from a static email→slug map — the gemaal.yaml
// identity map (see pkg/gemaalcfg). Email lookup is case-insensitive;
// mail addressing is, and an SSO session name's casing is not evidence
// of anything.
type MapResolver map[string]string

// Resolve implements Resolver.
func (m MapResolver) Resolve(_ context.Context, email string) (string, error) {
	if slug, ok := m[email]; ok {
		return slug, nil
	}

	for candidate, slug := range m {
		if strings.EqualFold(candidate, email) {
			return slug, nil
		}
	}

	return "", fmt.Errorf("email %q has no slug in the identity map", email)
}
