package authn

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
)

// zitadelRolesClaim is where Zitadel's userinfo carries role
// assertions: {roleKey: {orgID: orgDomain}}.
const zitadelRolesClaim = "urn:zitadel:iam:org:project:roles"

// UserinfoEnricher fills the identity gaps a gateway session leaves.
//
// The Envoy gateway's browser tokens deliberately carry no email and
// no role assertions (the estate's cookie-size ruling), so TokenReview
// yields a bare numeric subject with empty groups: the panel renders a
// raw Zitadel user ID, and authorization can never see an admin group
// (issue #27). One userinfo call with the SAME bearer token recovers
// both — the claims live server-side regardless of what the cookie
// carries.
type UserinfoEnricher struct {
	// URL is the OIDC userinfo endpoint,
	// e.g. "https://auth.example.com/oidc/v1/userinfo".
	URL string

	// Client is the HTTP client; http.DefaultClient when nil.
	Client *http.Client
}

// Enrich returns the identity with email and role-derived groups
// filled from userinfo. Best-effort by design: any failure returns the
// identity unchanged — enrichment widens what a caller can prove, so
// degrading to the un-enriched identity is a narrowing, never a
// bypass.
func (e *UserinfoEnricher) Enrich(ctx context.Context, token string, identity Identity) Identity {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.URL, nil)
	if err != nil {
		return identity
	}

	req.Header.Set("Authorization", "Bearer "+token)

	client := e.Client
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return identity
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return identity
	}

	var claims map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&claims); err != nil {
		return identity
	}

	if identity.Email == "" {
		if email, ok := claims["email"].(string); ok {
			identity.Email = email
		}
	}

	// Role KEYS become groups, verbatim ("{ns}:{role}" per the
	// role-spine ProjectRole convention) — adminGroups may name them
	// directly alongside the kube-token group shapes.
	if roles, ok := claims[zitadelRolesClaim].(map[string]any); ok {
		for key := range roles {
			if !slices.Contains(identity.Groups, key) {
				identity.Groups = append(identity.Groups, key)
			}
		}

		slices.Sort(identity.Groups)
	}

	return identity
}

// looksEnrichable reports whether an identity is a candidate for
// userinfo enrichment: humans arriving through the gateway have a bare
// numeric subject (no "@"), while kube exec-auth humans and CI machine
// users already carry an email-shaped subject and their token groups.
func looksEnrichable(identity Identity) bool {
	return identity.Email == "" && !strings.Contains(identity.Subject, "@")
}
