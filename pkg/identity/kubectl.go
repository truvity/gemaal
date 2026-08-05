package identity

import (
	"context"
	"encoding/json"
	"fmt"
)

// KubectlDriver asks the cluster who the ambient credentials belong to:
// `kubectl auth whoami -o json` (SelfSubjectReview, stable since
// Kubernetes 1.28). kubectl is the abstraction boundary on purpose —
// kubeconfig discovery, exec credential plugins (browser SSO, token
// cache, refresh) and API transport are its job; this package never
// parses kubeconfig and never imports client-go.
type KubectlDriver struct {
	// Kubecontext is the kubeconfig context name (empty = current).
	Kubecontext string

	// Kubeconfig overrides the ambient kubeconfig (kubectl --kubeconfig).
	Kubeconfig string

	// Runner executes kubectl; ExecRunner when nil.
	Runner Runner
}

// Name implements Driver.
func (KubectlDriver) Name() string { return "kubectl" }

// Email implements Driver. A kubectl that fails (not installed, not
// logged in, no such context) is no evidence; so is a username that is
// not an email (a certificate CN, a service account) — both let the
// chain try the next driver. Output that cannot be parsed is breakage
// and fails the chain.
func (d KubectlDriver) Email(ctx context.Context) (string, error) {
	out, err := runner(d.Runner).Output(ctx, whoAmIArgs(d.Kubecontext, d.Kubeconfig)...)
	if err != nil {
		return "", fmt.Errorf("%w (%v)", ErrNoEvidence, err)
	}

	return emailFromWhoAmI([]byte(out))
}

// whoAmIArgs builds the `kubectl auth whoami -o json` argv — shared by
// the evidence driver (username) and the interim groups resolver
// (groups), so the two views of the same SelfSubjectReview can never
// query it differently.
func whoAmIArgs(kubecontext, kubeconfig string) []string {
	argv := []string{"kubectl"}
	if kubecontext != "" {
		argv = append(argv, "--context", kubecontext)
	}

	if kubeconfig != "" {
		argv = append(argv, "--kubeconfig", kubeconfig)
	}

	return append(argv, "auth", "whoami", "-o", "json")
}

// selfSubjectReview is the JSON shape of `kubectl auth whoami -o json`.
type selfSubjectReview struct {
	Status struct {
		UserInfo struct {
			Username string   `json:"username"`
			Groups   []string `json:"groups"`
		} `json:"userInfo"`
	} `json:"status"`
}

// emailFromWhoAmI decodes the SelfSubjectReview and asserts the username
// is an email. Split from Email for testability without a cluster.
func emailFromWhoAmI(data []byte) (string, error) {
	var review selfSubjectReview
	if err := json.Unmarshal(data, &review); err != nil {
		return "", fmt.Errorf("parse kubectl auth whoami output: %w", err)
	}

	username := review.Status.UserInfo.Username
	if username == "" {
		return "", fmt.Errorf("%w (kubectl auth whoami returned no username — not authenticated?)", ErrNoEvidence)
	}

	if err := validEmail(username); err != nil {
		return "", fmt.Errorf("%w (cluster username %v — not a person's login)", ErrNoEvidence, err)
	}

	return username, nil
}
