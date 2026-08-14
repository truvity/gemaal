// Package authn turns a request's Authorization header into an Identity.
//
// Two kinds of callers reach the mutating RPCs:
//
//   - workloads, presenting a Kubernetes service-account token. These are
//     authenticated with a TokenReview against the cluster's own API —
//     the nats-auth-callout pattern — so the API server, not this
//     service, is the authority on what the token means.
//   - humans, arriving through the gateway. Envoy's SecurityPolicy runs
//     the OIDC login, validates the JWT against the issuer's JWKS, and
//     forwards the access token in the Authorization header (roster's
//     header exactly). This package reads the claims off that forwarded
//     token; it does not re-validate the signature — the gateway just
//     did, and the panel/API is only reachable through it. (Roster
//     re-verifies as defense in depth; adopting the same here is a
//     recorded follow-up, not a design disagreement.)
//
// The chain tries TokenReview first when a reviewer is wired; a token
// the cluster does not recognize falls through to claim parsing. A
// FAILING reviewer (API server unreachable) fails authentication closed
// rather than falling through to the weaker parse.
package authn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Method says which authority answered for the identity.
const (
	MethodTokenReview = "tokenreview"
	MethodGatewayJWT  = "gateway-jwt"
)

// ErrNoCredentials is returned when the request carries no bearer token.
var ErrNoCredentials = errors.New("authn: no bearer token presented")

// Identity is who the caller is, as far as this service is concerned.
type Identity struct {
	// Subject is the username: "system:serviceaccount:<ns>:<name>" for a
	// workload, the token's sub/email for a human.
	Subject string

	// Email is the human's email; empty for workloads.
	Email string

	// Groups carries the caller's groups: token-review groups for a
	// workload, the OIDC groups claim for a human.
	Groups []string

	// Method records which authority answered.
	Method string
}

// InAny reports whether the identity holds any of the given groups.
func (i Identity) InAny(groups []string) bool {
	for _, g := range groups {
		if g != "" && slices.Contains(i.Groups, g) {
			return true
		}
	}

	return false
}

// TokenReviewer validates a token against the cluster.
type TokenReviewer interface {
	// Review returns the identity and true when the cluster
	// authenticated the token; false (no error) when it did not.
	Review(ctx context.Context, token string) (Identity, bool, error)
}

// Authenticator resolves an Authorization header to an Identity.
type Authenticator struct {
	// Reviewer is the TokenReview client; nil skips straight to claim
	// parsing (out-of-cluster development).
	Reviewer TokenReviewer

	// GroupsClaim names the OIDC claim carrying groups ("groups").
	GroupsClaim string

	// Enricher, when set, fills email and role-derived groups from the
	// OIDC userinfo endpoint for identities that arrive bare (gateway
	// sessions carry no email/role claims — the cookie-size ruling).
	Enricher *UserinfoEnricher
}

// Authenticate implements the chain.
func (a *Authenticator) Authenticate(ctx context.Context, authorization string) (Identity, error) {
	token, ok := bearerToken(authorization)
	if !ok {
		return Identity{}, ErrNoCredentials
	}

	if a.Reviewer != nil {
		identity, authenticated, err := a.Reviewer.Review(ctx, token)
		if err != nil {
			// Fail closed: an unreachable authority is not a license to
			// downgrade to the weaker parse.
			return Identity{}, fmt.Errorf("token review: %w", err)
		}

		if authenticated {
			if a.Enricher != nil && looksEnrichable(identity) {
				identity = a.Enricher.Enrich(ctx, token, identity)
			}

			return identity, nil
		}
	}

	return identityFromJWT(token, a.GroupsClaim)
}

// bearerToken pulls the credential out of an Authorization header.
func bearerToken(header string) (string, bool) {
	const scheme = "Bearer "

	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}

	value := strings.TrimSpace(header[len(scheme):])

	return value, value != ""
}

// identityFromJWT reads the claims off a gateway-forwarded JWT. The
// payload is decoded, not verified — see the package comment.
func identityFromJWT(token, groupsClaim string) (Identity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, errors.New("authn: token is not a JWT and the cluster does not know it")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Identity{}, fmt.Errorf("authn: decode JWT payload: %w", err)
	}

	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Identity{}, fmt.Errorf("authn: parse JWT claims: %w", err)
	}

	identity := Identity{
		Subject: stringClaim(claims, "sub"),
		Email:   stringClaim(claims, "email"),
		Groups:  stringsClaim(claims, groupsClaim),
		Method:  MethodGatewayJWT,
	}

	if identity.Email != "" && identity.Subject == "" {
		identity.Subject = identity.Email
	}

	if identity.Subject == "" && identity.Email == "" && len(identity.Groups) == 0 {
		return Identity{}, errors.New("authn: token carries no identity claims")
	}

	return identity, nil
}

func stringClaim(claims map[string]any, name string) string {
	value, _ := claims[name].(string)

	return value
}

// stringsClaim normalizes the shapes a groups claim arrives in:
// providers disagree about whether a single group is a string or a
// one-element array.
func stringsClaim(claims map[string]any, name string) []string {
	switch value := claims[name].(type) {
	case string:
		return []string{value}
	case []any:
		out := make([]string, 0, len(value))

		for _, item := range value {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}

		return out
	default:
		return nil
	}
}
