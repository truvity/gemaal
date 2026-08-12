// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package engine

import (
	"context"
	"errors"
	"fmt"
)

// RuleDecommission names the explicit end-of-life rule: a caller said
// "done", not a TTL firing.
const RuleDecommission Rule = "decommission"

// ErrNoSuchTenant reports a Decommission target with no releases in the
// live view — already gone, or never there. Distinct from an execution
// failure: there is nothing to do, and the caller should know which.
var ErrNoSuchTenant = errors.New("no such tenant")

// Decommission plans and applies the deletion of ONE tenant's release
// ring, now.
//
// It is the explicit end-of-life path, so the tenant's own keep-until
// does NOT hold it — a keep-until exists to stop the TTL janitor, and
// the caller decommissioning their tenant is the opposite of wanting it
// kept. Everything else that guards a sweep guards this too:
//
//   - the view is enumerated live, so only releases that exist are
//     acted on (an absent half of the ring is skipped, not an error);
//   - Apply re-authorizes every action against the configured reach —
//     a namespace outside the tier selection refuses, whatever the
//     caller asked;
//   - ring3 still drains before ring2 (ringOrder), and confirm=false
//     dry-runs exactly like a sweep.
func (e *Engine) Decommission(ctx context.Context, namespace, release string, confirm bool) (*SweepRecord, error) {
	view, err := e.View(ctx)
	if err != nil {
		return nil, err
	}

	plan := &Plan{GeneratedAt: view.At}
	for i := range view.Namespaces {
		plan.Namespaces = append(plan.Namespaces, view.Namespaces[i].Name)
	}

	var target *TenantState

	for i := range view.Tenants {
		t := &view.Tenants[i]
		if t.Tenant.Namespace == namespace && t.Tenant.Release == release {
			target = t

			break
		}
	}

	if target == nil {
		return nil, fmt.Errorf("%w: %s/%s has no releases in the live view", ErrNoSuchTenant, namespace, release)
	}

	plan.Tenants = []TenantState{*target}

	for _, rel := range target.Releases {
		plan.Delete = append(plan.Delete, Action{
			Item: Item{
				Kind:         KindHelmRelease,
				Tenant:       target.Tenant,
				ID:           rel.Namespace + "/" + rel.Name,
				LastActivity: rel.Updated,
				Age:          view.At.Sub(rel.Updated),
				Members:      1,
			},
			Rule:   RuleDecommission,
			Order:  ringOrder(rel.Name),
			Reason: "explicit decommission — the caller declared the tenant end-of-life; its own keep-until does not hold it",
		})
	}

	return e.Apply(ctx, plan, confirm, "decommission")
}
