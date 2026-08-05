// Package identity answers "who is calling" as an EMAIL address, and
// maps that email to the short slug the organization knows the person by.
//
// The split is deliberate (docs/design.md, "Identity drivers"):
//
//   - EVIDENCE DRIVERS run client-side and extract an email, nothing
//     more: env (an explicit override — always wins, what CI sets),
//     kubectl (the authenticated username from `kubectl auth whoami`),
//     aws (the SSO role session name from the STS caller ARN).
//   - RESOLUTION maps email → slug and is somebody else's authority: the
//     gemaal service's Resolve RPC, rendered from the same personnel
//     source that feeds cluster RBAC. Until a deployment has one,
//     MapResolver serves the identical mapping from gemaal.yaml. Either
//     way, clients present evidence — they never invent the mapping.
//
// Drivers are chained (see Chain): clear precedence, first email wins,
// every consulted driver leaves a trail.
package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrNoEvidence reports that a driver's environment holds no personal
// identity evidence of its kind — an unset variable, a tool that is not
// logged in, a principal that is not a person. The chain records it and
// moves on. Any OTHER error is breakage or a bad explicit value, and
// stops the chain instead of silently switching identities.
var ErrNoEvidence = errors.New("no evidence")

// Driver is one source of identity evidence.
type Driver interface {
	// Name identifies the driver in resolutions and error trails.
	Name() string

	// Email extracts the caller's email from this driver's environment.
	// Errors wrapping ErrNoEvidence mean "nothing here, ask the next
	// driver"; anything else is fatal to the whole chain.
	Email(ctx context.Context) (string, error)
}

// Static is a Driver with a fixed answer — the seam for callers that
// already hold the email (a flag, a previous resolution) and for tests.
type Static string

// Name implements Driver.
func (Static) Name() string { return "static" }

// Email implements Driver. An empty Static has no evidence.
func (s Static) Email(context.Context) (string, error) {
	if s == "" {
		return "", fmt.Errorf("%w (empty)", ErrNoEvidence)
	}

	return string(s), nil
}

// validEmail asserts a piece of evidence is shaped like an email —
// drivers extract emails, not arbitrary principal names.
func validEmail(candidate string) error {
	local, domain, ok := strings.Cut(candidate, "@")
	if !ok || local == "" || domain == "" {
		return fmt.Errorf("%q is not an email", candidate)
	}

	return nil
}
