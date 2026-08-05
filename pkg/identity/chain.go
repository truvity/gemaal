package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Chain is an ordered list of evidence drivers: clear precedence, first
// email wins. There is no consensus step on purpose — two drivers CAN
// know the caller as different people (say, a shared robot cluster login
// but a personal AWS session), and the order, not a vote, decides.
type Chain []Driver

// DefaultChain is the conventional evidence order: the explicit override
// first (env — what CI sets, and what always wins), then the cluster's
// view of the caller (kubectl), then the AWS SSO session (aws).
func DefaultChain(kubecontext, kubeconfig string) Chain {
	return Chain{
		EnvDriver{},
		KubectlDriver{Kubecontext: kubecontext, Kubeconfig: kubeconfig},
		AWSDriver{},
	}
}

// Attempt records one consulted driver.
type Attempt struct {
	// Driver is the driver's Name.
	Driver string

	// Email is the extracted evidence; empty when Err is set.
	Email string

	// Err is why this driver did not answer: wrapping ErrNoEvidence when
	// its environment had nothing to say, anything else when it broke.
	Err error
}

// Resolution is a chain's outcome: the email, which driver produced it,
// and the full trail of everything consulted on the way — whoami-grade
// observability for a question that is asked rarely and debugged often.
type Resolution struct {
	// Email is the resolved evidence.
	Email string

	// Driver is the Name of the driver that answered.
	Driver string

	// Attempts lists every consulted driver in order, the answering one
	// last.
	Attempts []Attempt
}

// Trail renders the attempts as one human-readable line.
func (r Resolution) Trail() string {
	parts := make([]string, 0, len(r.Attempts))

	for _, a := range r.Attempts {
		if a.Err != nil {
			parts = append(parts, fmt.Sprintf("%s: %v", a.Driver, a.Err))

			continue
		}

		parts = append(parts, fmt.Sprintf("%s: %s", a.Driver, a.Email))
	}

	return strings.Join(parts, "; ")
}

// Resolve consults the drivers in order and stops at the first email.
// Drivers whose environment holds no evidence (ErrNoEvidence) are
// recorded and skipped; any other driver failure stops the chain — a
// broken or misconfigured source must not hand the answer to the next
// one. The returned Resolution carries the attempt trail even on error.
func (c Chain) Resolve(ctx context.Context) (Resolution, error) {
	if len(c) == 0 {
		return Resolution{}, errors.New("identity: empty driver chain")
	}

	var res Resolution

	for _, d := range c {
		email, err := d.Email(ctx)

		switch {
		case err == nil:
			res.Attempts = append(res.Attempts, Attempt{Driver: d.Name(), Email: email})
			res.Email = email
			res.Driver = d.Name()

			return res, nil

		case errors.Is(err, ErrNoEvidence):
			res.Attempts = append(res.Attempts, Attempt{Driver: d.Name(), Err: err})

		default:
			res.Attempts = append(res.Attempts, Attempt{Driver: d.Name(), Err: err})

			return res, fmt.Errorf("identity evidence (%s): %w", d.Name(), err)
		}
	}

	return res, fmt.Errorf("no identity evidence: %s", res.Trail())
}
