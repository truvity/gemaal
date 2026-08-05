package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// DefaultGroupPrefix marks the per-employee group in a cluster token's
// groups claim: "emp:{slug}".
const DefaultGroupPrefix = "emp:"

// KubectlGroupsResolver is the sanctioned INTERIM email→slug resolution
// for deployments that commit no people data: stateless, it asks the
// cluster itself (`kubectl auth whoami -o json`) and reads the slug out
// of the caller's own prefixed token group. The claim is rendered from
// the same personnel source that will feed the gemaal service's Resolve
// RPC — the end state, and the authority this resolver stands in for.
// It plugs the Resolver seam the RPC client will later fill; retirement
// is staged (swap the implementation, callers stay put), never silent.
// Promoted from bar#488's harness, where the pattern proved out.
//
// CI never reaches it by construction: CI sets GEMAAL_NAMESPACE, which
// wins the namespace ladder before any resolver is consulted.
type KubectlGroupsResolver struct {
	// Kubecontext is the kubeconfig context name (empty = current).
	Kubecontext string

	// Kubeconfig overrides the ambient kubeconfig (kubectl --kubeconfig).
	Kubeconfig string

	// GroupPrefix marks the slug-bearing group; DefaultGroupPrefix when
	// empty.
	GroupPrefix string

	// Runner executes kubectl; ExecRunner when nil.
	Runner Runner
}

// Resolve implements Resolver. The email argument is the chain's
// evidence; the slug still comes from the token's groups — one live
// source instead of a committed copy. Unlike the evidence driver, a
// failing kubectl is a hard error here: resolution was already reached,
// and failing loudly beats resolving as somebody else.
func (r KubectlGroupsResolver) Resolve(ctx context.Context, email string) (string, error) {
	out, err := runner(r.Runner).Output(ctx, whoAmIArgs(r.Kubecontext, r.Kubeconfig)...)
	if err != nil {
		return "", fmt.Errorf("kubectl auth whoami: %w", err)
	}

	return slugFromGroups([]byte(out), email, r.groupPrefix())
}

func (r KubectlGroupsResolver) groupPrefix() string {
	if r.GroupPrefix != "" {
		return r.GroupPrefix
	}

	return DefaultGroupPrefix
}

// slugFromGroups extracts the exactly-one prefixed group from a
// SelfSubjectReview document — zero matches means the session is not an
// employee's, two means an ambiguous identity, and both refuse rather
// than guess. Split from Resolve for testability without a cluster.
func slugFromGroups(data []byte, email, prefix string) (string, error) {
	var review selfSubjectReview
	if err := json.Unmarshal(data, &review); err != nil {
		return "", fmt.Errorf("parse kubectl auth whoami output: %w", err)
	}

	var slugs []string

	for _, g := range review.Status.UserInfo.Groups {
		if slug, ok := strings.CutPrefix(g, prefix); ok && slug != "" {
			slugs = append(slugs, slug)
		}
	}

	switch len(slugs) {
	case 1:
		return slugs[0], nil
	case 0:
		return "", fmt.Errorf(
			"cluster token for %s carries no %s{slug} group — not an employee session? "+
				"(set GEMAAL_NAMESPACE to override)", email, prefix)
	default:
		return "", fmt.Errorf(
			"cluster token for %s carries %d %s{slug} groups (%s) — ambiguous identity, "+
				"set GEMAAL_NAMESPACE to override", email, len(slugs), prefix, strings.Join(slugs, ", "))
	}
}
