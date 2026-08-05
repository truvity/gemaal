package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// AWSDriver reads the SSO role session name from the STS caller identity
// ARN: under AWS IAM Identity Center an assumed permission-set role is
// sessioned by the user's login, which is their email —
//
//	arn:aws:sts::123456789012:assumed-role/AWSReservedSSO_X_y/j.doe@example.com
//	                                                          ^^^^^^^^^^^^^^^^^
//
// The aws CLI is the abstraction boundary, exactly like kubectl in
// KubectlDriver: profile files, SSO token cache and refresh are its job.
type AWSDriver struct {
	// Profile selects an aws CLI profile (empty = the ambient default).
	Profile string

	// Runner executes the aws CLI; ExecRunner when nil.
	Runner Runner
}

// Name implements Driver.
func (AWSDriver) Name() string { return "aws" }

// Email implements Driver. An aws CLI that fails (not installed, not
// logged in) is no evidence; so is a caller identity that carries no
// email-shaped session name (an IAM user, a machine role) — both let
// the chain move on. Unparseable output is breakage and fails the chain.
func (d AWSDriver) Email(ctx context.Context) (string, error) {
	argv := []string{"aws", "sts", "get-caller-identity", "--output", "json"}
	if d.Profile != "" {
		argv = append(argv, "--profile", d.Profile)
	}

	out, err := runner(d.Runner).Output(ctx, argv...)
	if err != nil {
		return "", fmt.Errorf("%w (%v)", ErrNoEvidence, err)
	}

	var identity struct {
		Arn string `json:"Arn"`
	}

	if err := json.Unmarshal([]byte(out), &identity); err != nil {
		return "", fmt.Errorf("parse aws sts get-caller-identity output: %w", err)
	}

	return SessionEmailFromARN(identity.Arn)
}

// SessionEmailFromARN extracts the role session name from an STS
// assumed-role ARN and asserts it is an email. Errors wrap ErrNoEvidence:
// a non-SSO caller identity is a legitimately different shape, not
// breakage.
func SessionEmailFromARN(arn string) (string, error) {
	const marker = ":assumed-role/"

	idx := strings.Index(arn, marker)
	if idx < 0 {
		return "", fmt.Errorf("%w (caller ARN %q is not an assumed-role ARN — no SSO session name to read)", ErrNoEvidence, arn)
	}

	// The resource tail is ROLE-NAME/SESSION-NAME.
	_, session, ok := strings.Cut(arn[idx+len(marker):], "/")
	if !ok || session == "" {
		return "", fmt.Errorf("%w (caller ARN %q carries no role session name)", ErrNoEvidence, arn)
	}

	if err := validEmail(session); err != nil {
		return "", fmt.Errorf("%w (role session name %v — not an SSO user session)", ErrNoEvidence, err)
	}

	return session, nil
}
