package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// KeepUntilLayout is the label-safe timestamp encoding for keep-until —
// the same layout pkg/harness stamps (label values forbid ':', so
// RFC3339 cannot ride a label).
const KeepUntilLayout = "20060102T150405Z"

// View is the level-triggered world: everything re-derived from what
// exists, every tick. There is no state but the cluster.
type View struct {
	At         time.Time
	Namespaces []Namespace
	Tenants    []TenantState
}

// View builds the tenant view: tier-labeled namespaces, their releases
// grouped into ring pairs, and the ledger read off the release Secrets.
//
// Failure policy (a safety property, not an ergonomic one): namespace,
// release and ledger listing failures are all FATAL. Orphanhood and
// expiry are decided by this view, so an incomplete one would turn live
// or held tenants into deletion candidates.
func (e *Engine) View(ctx context.Context) (*View, error) {
	if e.Kube == nil || e.Helm == nil {
		return nil, errors.New("engine: kube and helm clients are required — release truth is not optional")
	}

	at := e.now()

	tiers := e.Config.TierValues()

	namespaces, err := e.Kube.ListTierNamespaces(ctx, e.Config.TierLabel, tiers)
	if err != nil {
		return nil, fmt.Errorf("list tier namespaces: %w", err)
	}

	sort.Slice(namespaces, func(i, j int) bool { return namespaces[i].Name < namespaces[j].Name })

	view := &View{At: at}

	for _, ns := range namespaces {
		// Belt and braces on top of the label selector: a namespace whose
		// tier is not configured is out of reach even if the client
		// returned it. Expansion can only narrow the reach, never widen it.
		if _, ok := e.Config.TierTTL(ns.Tier); !ok {
			continue
		}

		view.Namespaces = append(view.Namespaces, ns)

		tenants, err := e.namespaceTenants(ctx, ns)
		if err != nil {
			return nil, err
		}

		view.Tenants = append(view.Tenants, tenants...)
	}

	sort.Slice(view.Tenants, func(i, j int) bool {
		a, b := view.Tenants[i], view.Tenants[j]
		if a.Tenant.Namespace != b.Tenant.Namespace {
			return a.Tenant.Namespace < b.Tenant.Namespace
		}

		return a.Tenant.Release < b.Tenant.Release
	})

	return view, nil
}

// Find returns the tenant state for one identity, when the view holds it.
func (v *View) Find(namespace, release string) (TenantState, bool) {
	for i := range v.Tenants {
		if v.Tenants[i].Tenant.Namespace == namespace && v.Tenants[i].Tenant.Release == release {
			return v.Tenants[i], true
		}
	}

	return TenantState{}, false
}

// Live returns the live tenant set, keyed by identity — what artifact
// orphanhood is decided against.
func (v *View) Live() map[Tenant]bool {
	live := make(map[Tenant]bool, len(v.Tenants))
	for i := range v.Tenants {
		live[v.Tenants[i].Tenant] = true
	}

	return live
}

// namespaceTenants lists one namespace's releases, groups the ring
// pairs, and merges in the ledger labels.
func (e *Engine) namespaceTenants(ctx context.Context, ns Namespace) ([]TenantState, error) {
	releases, err := e.Helm.ListReleases(ctx, ns.Name)
	if err != nil {
		return nil, fmt.Errorf("list helm releases in %s: %w", ns.Name, err)
	}

	if len(releases) == 0 {
		return nil, nil
	}

	secrets, err := e.Kube.ReleaseSecrets(ctx, ns.Name)
	if err != nil {
		return nil, fmt.Errorf("list release secrets in %s: %w", ns.Name, err)
	}

	byRelease := make(map[string]ReleaseSecret, len(secrets))
	for _, s := range secrets {
		byRelease[s.Release] = s
	}

	groups := map[string][]Release{}

	for _, rel := range releases {
		install := installOf(rel.Name)
		groups[install] = append(groups[install], rel)
	}

	tenants := make([]TenantState, 0, len(groups))

	for install, members := range groups {
		// App before infra, deterministically.
		sort.Slice(members, func(i, j int) bool {
			if o1, o2 := ringOrder(members[i].Name), ringOrder(members[j].Name); o1 != o2 {
				return o1 < o2
			}

			return members[i].Name < members[j].Name
		})

		state := TenantState{
			Tenant:   Tenant{Namespace: ns.Name, Release: install},
			Tier:     ns.Tier,
			Releases: members,
		}

		for _, rel := range members {
			if rel.Updated.After(state.LastActivity) {
				state.LastActivity = rel.Updated
			}
		}

		e.mergeLedger(&state, byRelease)

		// Effective TTL: the ledger's own when present, the tier default
		// otherwise. The tier is configured — View only admits configured
		// tiers — so the default always exists.
		tierTTL, _ := e.Config.TierTTL(ns.Tier)

		if state.Ledger.TTL > 0 {
			state.TTL = state.Ledger.TTL
			state.TTLSource = "label"
		} else {
			state.TTL = tierTTL
			state.TTLSource = "tier-default"
		}

		tenants = append(tenants, state)
	}

	return tenants, nil
}

// mergeLedger reads the ledger off the ring pair's release Secrets. Per
// label, the app release's stamp wins over the infra release's — one
// deterministic rule, documented rather than discovered.
func (e *Engine) mergeLedger(state *TenantState, secrets map[string]ReleaseSecret) {
	ttlKey, keepKey, execKey := e.Config.Labels()

	value := func(key string) string {
		for _, rel := range state.Releases { // app first (sorted by ring order)
			if s, ok := secrets[rel.Name]; ok {
				if v, ok := s.Labels[key]; ok && v != "" {
					return v
				}
			}
		}

		return ""
	}

	if raw := value(ttlKey); raw != "" {
		ttl, err := time.ParseDuration(raw)
		if err != nil || ttl <= 0 {
			state.LedgerProblems = append(state.LedgerProblems,
				fmt.Sprintf("ttl label %q does not parse as a positive duration", raw))
		} else {
			state.Ledger.TTL = ttl
		}
	}

	if raw := value(keepKey); raw != "" {
		keep, err := parseKeepUntil(raw)
		if err != nil {
			state.LedgerProblems = append(state.LedgerProblems,
				fmt.Sprintf("keep-until label %q does not parse (want %s)", raw, KeepUntilLayout))
		} else {
			state.Ledger.KeepUntil = keep
		}
	}

	state.Ledger.ExecutionID = value(execKey)
}

// parseKeepUntil accepts the label-safe layout.
func parseKeepUntil(raw string) (time.Time, error) {
	return time.Parse(KeepUntilLayout, raw)
}

// FormatKeepUntil renders a keep-until stamp in the label-safe layout.
func FormatKeepUntil(t time.Time) string {
	return t.UTC().Format(KeepUntilLayout)
}

// StampKeepUntil writes the keep-until label onto every ring member's
// newest release Secret — either half surviving alone still carries the
// hold. A tenant with no releases cannot be stamped.
func (e *Engine) StampKeepUntil(ctx context.Context, tenant Tenant, until time.Time) error {
	secrets, err := e.Kube.ReleaseSecrets(ctx, tenant.Namespace)
	if err != nil {
		return fmt.Errorf("list release secrets in %s: %w", tenant.Namespace, err)
	}

	_, keepKey, _ := e.Config.Labels()
	stamp := map[string]string{keepKey: FormatKeepUntil(until)}

	stamped := 0

	for _, s := range secrets {
		if installOf(s.Release) != tenant.Release {
			continue
		}

		if err := e.Kube.LabelSecret(ctx, tenant.Namespace, s.Name, stamp); err != nil {
			return fmt.Errorf("label secret %s/%s: %w", tenant.Namespace, s.Name, err)
		}

		stamped++
	}

	if stamped == 0 {
		return fmt.Errorf("tenant %s has no release secrets to stamp", tenant)
	}

	e.log().InfoContext(ctx, "keep-until stamped",
		"namespace", tenant.Namespace,
		"release", tenant.Release,
		"keep_until", FormatKeepUntil(until),
		"secrets", stamped,
	)

	return nil
}
