// Package authn turns a request's bearer token into an Identity.
//
// Two kinds of callers reach the mutating RPCs:
//
//   - workloads, presenting a Kubernetes service-account token. These are
//     authenticated with a TokenReview against the cluster's own API —
//     the nats-auth-callout pattern — so the API server, not this
//     service, is the authority on what the token means.
//   - people, whose token access-issuer signed: forwarded by the console's
//     access-proxy, or presented directly by a CLI. The signature is
//     VERIFIED against the issuer's keys, with access-roster's own
//     identity package. Nothing here trusts a claim it has not checked,
//     because "only the gateway can reach this" is one NetworkPolicy edit
//     or one port-forward away from false.
//
// The chain tries TokenReview first when a reviewer is wired; a token the
// cluster does not recognize goes to the issuer. A FAILING reviewer (API
// server unreachable) fails authentication closed rather than falling
// through, and a token neither authority vouches for is refused.
package authn

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/truvity/access-roster/identity"
)

// Method says which authority answered for the identity.
const (
	MethodTokenReview = "tokenreview"
	MethodIssuer      = "issuer"
)

// ErrNoCredentials is returned when the request carries no bearer token.
var ErrNoCredentials = errors.New("authn: no bearer token presented")

// Identity is who the caller is, as far as this service is concerned.
type Identity struct {
	// Subject is the username: "system:serviceaccount:<ns>:<name>" for a
	// workload, the token's `sub` for a person (their address).
	Subject string

	// Email is the human's email; empty for workloads.
	Email string

	// Name is the person's display name, from the verified token; empty for
	// workloads. Display only — nothing authorizes on it.
	Name string

	// Groups carries the caller's groups: token-review groups for a
	// workload, the internal groups access-issuer put in a person's token.
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

// Verifier checks a token access-issuer signed. *identity.Issuer satisfies
// it; tests inject fakes.
type Verifier interface {
	Verify(ctx context.Context, token string) (identity.Verified, error)
}

// Authenticator resolves a bearer token to an Identity.
type Authenticator struct {
	// Reviewer is the TokenReview client; nil skips it (out-of-cluster
	// development).
	Reviewer TokenReviewer

	// Issuer verifies people's tokens; nil refuses every token the
	// cluster does not vouch for.
	Issuer Verifier
}

// Authenticate implements the chain. token is the bare credential, as
// identity.TokenFrom reads it off a request.
func (a *Authenticator) Authenticate(ctx context.Context, token string) (Identity, error) {
	if token == "" {
		return Identity{}, ErrNoCredentials
	}

	if a.Reviewer != nil {
		identity, authenticated, err := a.Reviewer.Review(ctx, token)
		if err != nil {
			// Fail closed: an unreachable authority is not a license to
			// try a different one.
			return Identity{}, fmt.Errorf("token review: %w", err)
		}

		if authenticated {
			return identity, nil
		}
	}

	if a.Issuer == nil {
		return Identity{}, errors.New("authn: the cluster does not know this token and no issuer is configured")
	}

	who, err := a.Issuer.Verify(ctx, token)
	if err != nil {
		return Identity{}, fmt.Errorf("issuer: %w", err)
	}

	return Identity{
		Subject: who.Subject,
		Email:   who.Email,
		Name:    who.Name,
		Groups:  who.Groups,
		Method:  MethodIssuer,
	}, nil
}
