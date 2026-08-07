package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// artifactGroup is a tenant-attributed set of artifacts, produced from a
// flat object/parameter listing.
type artifactGroup struct {
	tenant       Tenant
	id           string
	lastActivity time.Time
	members      int
	names        []string
	attributable bool
}

// Narrow filters a plan. Empty fields mean everything in scope.
type Narrow struct {
	Namespace string
	Release   string
}

func (n Narrow) matches(t Tenant) bool {
	if n.Namespace != "" && t.Namespace != n.Namespace {
		return false
	}

	if n.Release != "" && t.Release != n.Release {
		return false
	}

	return true
}

// Plan classifies everything in reach and returns what housekeeping
// would do. It mutates nothing.
func (e *Engine) Plan(ctx context.Context, narrow Narrow) (*Plan, error) {
	view, err := e.View(ctx)
	if err != nil {
		return nil, err
	}

	return e.planFromView(ctx, view, narrow)
}

// planFromView applies the garbage rules to an already-built view.
func (e *Engine) planFromView(ctx context.Context, view *View, narrow Narrow) (*Plan, error) {
	plan := &Plan{
		GeneratedAt: view.At,
		Grace:       e.Config.Grace.Std(),
		Tenants:     view.Tenants,
	}

	for _, ns := range view.Namespaces {
		plan.Namespaces = append(plan.Namespaces, ns.Name)
	}

	for i := range view.Tenants {
		if !narrow.matches(view.Tenants[i].Tenant) {
			continue
		}

		e.classifyTenant(view.At, &view.Tenants[i], plan)
	}

	// Artifacts are judged against the FULL live set, never the narrowed
	// one: narrowing must not turn a live tenant's artifacts into orphans.
	live := view.Live()

	e.sweepSSM(ctx, view.At, live, narrow, plan)
	e.sweepS3(ctx, view.At, live, narrow, plan)

	sortActions(plan.Delete)
	sortKept(plan.Keep)

	e.log().InfoContext(ctx, "housekeeping plan built",
		"namespaces", len(plan.Namespaces),
		"tenants", len(plan.Tenants),
		"delete", len(plan.Delete),
		"keep", len(plan.Keep),
		"problems", len(plan.Problems),
	)

	return plan, nil
}

// classifyTenant applies the uniform TTL rule to one tenant's whole ring.
func (e *Engine) classifyTenant(at time.Time, tenant *TenantState, plan *Plan) {
	age := tenant.Age(at)

	for _, rel := range tenant.Releases {
		item := Item{
			Kind:         KindHelmRelease,
			Tenant:       tenant.Tenant,
			ID:           rel.Namespace + "/" + rel.Name,
			LastActivity: rel.Updated,
			Age:          at.Sub(rel.Updated),
			Members:      1,
		}

		switch {
		case len(tenant.LedgerProblems) > 0:
			// A broken stamp holds the tenant: never delete on garbage
			// input. The problem is surfaced so somebody fixes the stamp.
			plan.Keep = append(plan.Keep, Kept{Item: item, Reason: KeepLedgerUnparseable})
			plan.Problems = append(plan.Problems, TargetProblem{
				Target: tenant.Tenant.String(),
				Err:    strings.Join(tenant.LedgerProblems, "; "),
			})
		case tenant.Held(at):
			plan.Keep = append(plan.Keep, Kept{Item: item, Reason: KeepUntilHolds})
		case age <= tenant.TTL:
			plan.Keep = append(plan.Keep, Kept{Item: item, Reason: KeepWithinTTL})
		default:
			plan.Delete = append(plan.Delete, Action{
				Item:  item,
				Rule:  RuleTTLExpired,
				Order: ringOrder(rel.Name),
				Reason: fmt.Sprintf("tier %s tenant idle %s exceeds ttl %s (%s); no keep-until holds",
					tenant.Tier, roundAge(age), tenant.TTL, tenant.TTLSource),
			})
		}
	}
}

func (e *Engine) sweepSSM(ctx context.Context, at time.Time, live map[Tenant]bool, narrow Narrow, plan *Plan) {
	for _, root := range e.Config.AllowList.SSMRoots {
		root := normalizedRoot(root)

		if e.SSM == nil {
			plan.Problems = append(plan.Problems, TargetProblem{Target: root, Err: "no ssm client configured"})

			continue
		}

		groups := map[string]*artifactGroup{}

		err := e.SSM.ListParameters(ctx, root, func(param Parameter) error {
			// A parameter outside the root cannot happen with a correct
			// client, but the plan does not trust the client: an
			// out-of-root name is dropped rather than attributed.
			rest, inRoot := strings.CutPrefix(param.Name, root)
			if !inRoot {
				return nil
			}

			group := artifactGroupFor(groups, rest, root)
			group.members++
			group.names = append(group.names, param.Name)

			if param.LastModified.After(group.lastActivity) {
				group.lastActivity = param.LastModified
			}

			return nil
		})
		if err != nil {
			plan.Problems = append(plan.Problems, TargetProblem{Target: root, Err: err.Error()})

			continue
		}

		e.classifyArtifacts(at, live, groups, KindSSMPrefix, narrow, plan)
	}
}

// sweepS3 covers the allow-listed test buckets. With no S3 client wired
// each configured bucket is recorded as a problem, so a plan that skips
// S3 says so instead of staying silent (defensive: the wiring builds a
// client whenever buckets are configured).
func (e *Engine) sweepS3(ctx context.Context, at time.Time, live map[Tenant]bool, narrow Narrow, plan *Plan) {
	for _, bucket := range e.Config.AllowList.S3Buckets {
		target := "s3://" + bucket

		if e.S3 == nil {
			plan.Problems = append(plan.Problems, TargetProblem{
				Target: target,
				Err:    "s3 sweep is stubbed off (no client wired — pending the shared test bucket)",
			})

			continue
		}

		groups := map[string]*artifactGroup{}

		err := e.S3.ListObjects(ctx, bucket, func(obj Object) error {
			group := artifactGroupFor(groups, obj.Key, "s3://"+bucket+"/")
			group.members++

			if obj.LastModified.After(group.lastActivity) {
				group.lastActivity = obj.LastModified
			}

			return nil
		})
		if err != nil {
			plan.Problems = append(plan.Problems, TargetProblem{Target: target, Err: err.Error()})

			continue
		}

		e.classifyArtifacts(at, live, groups, KindS3Prefix, narrow, plan)
	}
}

// artifactGroupFor buckets one raw key into its tenant prefix group.
func artifactGroupFor(groups map[string]*artifactGroup, rest, idPrefix string) *artifactGroup {
	ns, rel, ok := splitTenantPath(rest)
	key := ns + "/" + rel + "/"

	if !ok {
		key = topSegment(rest) + "/"
	}

	group, exists := groups[key]
	if !exists {
		group = &artifactGroup{
			tenant:       Tenant{Namespace: ns, Release: rel},
			id:           idPrefix + key,
			attributable: ok,
		}
		groups[key] = group
	}

	return group
}

// classifyArtifacts applies the orphan-plus-grace rule.
//
// Note the deliberate conservatism, carried over from the donor: a
// tenant whose releases are being deleted by THIS plan still counts as
// live, so its artifacts are kept. They orphan once the release is
// actually gone and are collected by a later tick. No artifact is ever
// deleted on the strength of a deletion that has not happened yet.
func (e *Engine) classifyArtifacts(
	at time.Time,
	live map[Tenant]bool,
	groups map[string]*artifactGroup,
	kind Kind,
	narrow Narrow,
	plan *Plan,
) {
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	grace := e.Config.Grace.Std()

	for _, key := range keys {
		group := groups[key]

		if group.attributable && !narrow.matches(group.tenant) {
			continue
		}

		sort.Strings(group.names)

		item := Item{
			Kind:         kind,
			Tenant:       group.tenant,
			ID:           group.id,
			LastActivity: group.lastActivity,
			Age:          at.Sub(group.lastActivity),
			Members:      group.members,
			Names:        group.names,
		}

		switch {
		case !group.attributable:
			plan.Keep = append(plan.Keep, Kept{Item: item, Reason: KeepUnknownTenantShape})
		case !e.tierSelected(group.tenant.Namespace, plan):
			plan.Keep = append(plan.Keep, Kept{Item: item, Reason: KeepOutOfReach})
		case live[group.tenant]:
			plan.Keep = append(plan.Keep, Kept{Item: item, Reason: KeepReleaseLive})
		case item.Age <= grace:
			plan.Keep = append(plan.Keep, Kept{Item: item, Reason: KeepWithinGrace})
		default:
			plan.Delete = append(plan.Delete, Action{
				Item: item,
				Rule: RuleOrphanGrace,
				Reason: fmt.Sprintf("tenant %s has no helm release; idle %s exceeds grace %s",
					group.tenant, roundAge(item.Age), grace),
			})
		}
	}
}

// tierSelected reports whether a namespace was inside this run's reach —
// the tier-labeled set the view enumerated. An artifact under a
// namespace outside it (deleted namespace, foreign occupant) is only
// collectable when the namespace is GONE from the cluster but was
// tier-shaped state under an allow-listed root; conservatively, only
// namespaces the view saw are in reach, everything else is reported as
// out-of-reach and left alone.
//
// A deleted tenant namespace therefore keeps its orphaned artifacts —
// visible in every plan as out-of-reach — until an operator removes them
// or the namespace row returns. Deliberate for v1: collecting artifacts
// for namespaces nobody can enumerate anymore would mean trusting the
// artifact path alone to decide tenancy.
func (e *Engine) tierSelected(namespace string, plan *Plan) bool {
	for _, ns := range plan.Namespaces {
		if ns == namespace {
			return true
		}
	}

	return false
}

// normalizedRoot returns an SSM root with exactly one leading and one
// trailing slash.
func normalizedRoot(root string) string {
	return "/" + strings.Trim(root, "/") + "/"
}
