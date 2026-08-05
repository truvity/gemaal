package identity

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// EnvEmail is the explicit override variable — what CI sets, and what an
// operator exports to speak for themselves without any lookup.
const EnvEmail = "GEMAAL_EMAIL"

// EnvDriver reads the explicit override variable. It goes first in the
// default chain: a caller that SAYS who it is always wins over anything
// the environment could be interrogated for.
type EnvDriver struct {
	// Var is the variable read; EnvEmail when empty.
	Var string
}

// Name implements Driver.
func (EnvDriver) Name() string { return "env" }

// Email implements Driver. An unset variable is no evidence; a set but
// non-email value is a fatal configuration error — an explicit override
// must never be silently ignored in favor of a guessed identity.
func (d EnvDriver) Email(context.Context) (string, error) {
	name := d.Var
	if name == "" {
		name = EnvEmail
	}

	email := strings.TrimSpace(os.Getenv(name))
	if email == "" {
		return "", fmt.Errorf("%w (%s unset)", ErrNoEvidence, name)
	}

	if err := validEmail(email); err != nil {
		return "", fmt.Errorf("%s: %w — fix or unset the override", name, err)
	}

	return email, nil
}
