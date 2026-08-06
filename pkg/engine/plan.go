package engine

import (
	"sort"
	"strings"
	"time"
)

const (
	// KindHelmRelease is one Helm release (one half of a ring pair).
	KindHelmRelease Kind = "helm-release"
	// KindS3Prefix is a tenant's "<ns>/<rel>/" prefix in a test bucket.
	KindS3Prefix Kind = "s3-prefix"
	// KindSSMPrefix is a tenant's "/test/<ns>/<rel>/" parameter subtree.
	KindSSMPrefix Kind = "ssm-prefix"

	// RuleTTLExpired — the tenant's TTL (from last activity) has passed
	// and no keep-until holds it.
	RuleTTLExpired Rule = "ttl-expired"
	// RuleOrphanGrace — the artifact's tenant has no Helm release and the
	// artifact has been idle longer than the grace period.
	RuleOrphanGrace Rule = "orphaned-past-grace"

	// KeepReleaseLive — the artifact's tenant still has a Helm release.
	KeepReleaseLive KeepReason = "release-live"
	// KeepWithinTTL — a tenant that has not yet outlived its TTL.
	KeepWithinTTL KeepReason = "within-ttl"
	// KeepUntilHolds — a keep-until label in the future holds the tenant,
	// whatever its TTL says.
	KeepUntilHolds KeepReason = "keep-until-holds"
	// KeepWithinGrace — an orphan that has not yet been idle long enough.
	KeepWithinGrace KeepReason = "within-grace"
	// KeepLedgerUnparseable — a ledger label or a release's "updated"
	// timestamp refused to parse. The safe direction is a hold — never
	// "assume it is old" — and the problem is recorded so an operator
	// sees the broken stamp.
	KeepLedgerUnparseable KeepReason = "ledger-unparseable"
	// KeepOutOfReach — an artifact whose tenant namespace is not selected
	// by the tier configuration. Reported, never deleted, so the
	// operator can see the root has occupants the engine is not
	// responsible for.
	KeepOutOfReach KeepReason = "out-of-reach"
	// KeepUnknownTenantShape — an object in an allow-listed root that
	// does not decompose into (namespace, release). Never deleted: the
	// engine only collects things it can attribute to a tenant.
	KeepUnknownTenantShape KeepReason = "unknown-tenant-shape"

	// infraSuffix is the ring2 release suffix. Components on this model
	// install as a PAIR of Helm releases: "<rel>-infra" (ring2:
	// databases, queues, claims) and "<rel>" (ring3: the application).
	// Both belong to one tenant; ring3 is uninstalled before ring2 or
	// the ring2 finalizers block on pods that are still running.
	infraSuffix = "-infra"
)

type (
	// Kind is the type of artifact an Item describes.
	Kind string

	// Rule names the garbage rule that matched.
	Rule string

	// KeepReason names why an item was left alone.
	KeepReason string

	// Tenant is the tenancy identity: everything else derives from it.
	Tenant struct {
		Namespace string `json:"namespace" yaml:"namespace"`
		Release   string `json:"release"   yaml:"release"`
	}

	// Ledger is the parsed label state of one tenant: what the install
	// stamped on its release Secrets.
	Ledger struct {
		// TTL is the tenant's own ttl label; zero means absent (the
		// tier default applies).
		TTL time.Duration `json:"ttl" yaml:"ttl"`

		// KeepUntil holds the tenant until an absolute time; zero means
		// absent.
		KeepUntil time.Time `json:"keep_until" yaml:"keepUntil"`

		// ExecutionID tags the test run that last touched the tenant.
		ExecutionID string `json:"execution_id" yaml:"executionID"`
	}

	// TenantState is one watched installation with its derived state:
	// the console's row and the plan's input.
	TenantState struct {
		Tenant Tenant `json:"tenant" yaml:"tenant"`

		// Tier is the namespace's tier label value.
		Tier string `json:"tier" yaml:"tier"`

		// Releases are the ring members, app before infra.
		Releases []Release `json:"releases" yaml:"releases"`

		// Ledger is the merged label state (app release wins over infra).
		Ledger Ledger `json:"ledger" yaml:"ledger"`

		// LedgerProblems records unparseable tenant state: ledger labels
		// or a release's "updated" timestamp. A tenant with any is HELD,
		// never deleted.
		LedgerProblems []string `json:"ledger_problems,omitempty" yaml:"ledgerProblems,omitempty"`

		// LastActivity is the newest last-deployed time across the ring:
		// an upgrade of either half is activity on the tenant.
		LastActivity time.Time `json:"last_activity" yaml:"lastActivity"`

		// TTL is the effective time-to-live: the ledger's when present,
		// the tier default otherwise.
		TTL time.Duration `json:"effective_ttl" yaml:"effectiveTTL"`

		// TTLSource says which: "label" or "tier-default".
		TTLSource string `json:"ttl_source" yaml:"ttlSource"`
	}

	// Item is one addressable unit the plan speaks about.
	Item struct {
		Kind   Kind   `json:"kind"   yaml:"kind"`
		Tenant Tenant `json:"tenant" yaml:"tenant"`

		// ID is the fully qualified address, for logs and for humans:
		// "emp-jdoe/myapp" for a release, "s3://devel-test/emp-jdoe/myapp/"
		// for a prefix, "/test/emp-jdoe/myapp/" for an SSM subtree.
		ID string `json:"id" yaml:"id"`

		// LastActivity is the newest timestamp observable for the item.
		LastActivity time.Time `json:"last_activity" yaml:"lastActivity"`

		// Age is now-LastActivity at plan time.
		Age time.Duration `json:"age" yaml:"age"`

		// Members counts the underlying objects (S3 keys, SSM
		// parameters). One for a Helm release.
		Members int `json:"members" yaml:"members"`

		// Names carries the underlying object names when they are few
		// enough to be worth naming (SSM parameters).
		Names []string `json:"names,omitempty" yaml:"names,omitempty"`
	}

	// Action is a deletion the plan proposes, with the rule that
	// produced it. Order sequences deletions that must not race: ring3
	// (order 0) goes before ring2 (order 1) so app pods drain before the
	// infra finalizers run.
	Action struct {
		Item   Item   `json:"item"   yaml:"item"`
		Rule   Rule   `json:"rule"   yaml:"rule"`
		Reason string `json:"reason" yaml:"reason"`
		Order  int    `json:"order"  yaml:"order"`
	}

	// Kept is an item the plan looked at and left alone, with the
	// reason. A plan that only lists deletions is not auditable — the
	// operator needs to see that the held tenant WAS considered and
	// spared.
	Kept struct {
		Item   Item       `json:"item"   yaml:"item"`
		Reason KeepReason `json:"reason" yaml:"reason"`
	}

	// TargetProblem records a root the plan could not fully enumerate.
	// Artifact enumeration failures are non-fatal because they can only
	// ever REMOVE candidates: orphanhood is decided by the Helm release
	// set, which is enumerated completely or the plan fails outright.
	TargetProblem struct {
		Target string `json:"target" yaml:"target"`
		Err    string `json:"error"  yaml:"error"`
	}

	// Plan is what housekeeping would do. It is inert: producing one
	// mutates nothing, and Apply is the only thing that acts on it.
	Plan struct {
		GeneratedAt time.Time       `json:"generated_at"       yaml:"generatedAt"`
		Grace       time.Duration   `json:"grace"              yaml:"grace"`
		Namespaces  []string        `json:"namespaces"         yaml:"namespaces"`
		Tenants     []TenantState   `json:"tenants"            yaml:"tenants"`
		Delete      []Action        `json:"delete"             yaml:"delete"`
		Keep        []Kept          `json:"keep"               yaml:"keep"`
		Problems    []TargetProblem `json:"problems,omitempty" yaml:"problems,omitempty"`
	}
)

// String renders the tenant as namespace/release.
func (t Tenant) String() string { return t.Namespace + "/" + t.Release }

// Age is now minus last activity.
func (s TenantState) Age(at time.Time) time.Duration { return at.Sub(s.LastActivity) }

// Held reports whether a keep-until (or an unparseable ledger, which is
// held on principle) protects the tenant at the given instant.
func (s TenantState) Held(at time.Time) bool {
	return len(s.LedgerProblems) > 0 || (!s.Ledger.KeepUntil.IsZero() && at.Before(s.Ledger.KeepUntil))
}

// Expired reports whether the uniform TTL rule fires: TTL from last
// activity has passed and no keep-until holds. Keep-until takes
// precedence over TTL — that is the human override's whole point.
func (s TenantState) Expired(at time.Time) bool {
	return !s.Held(at) && s.Age(at) > s.TTL
}

// Empty reports whether the plan would delete nothing.
func (p *Plan) Empty() bool { return len(p.Delete) == 0 }

// installOf strips the ring2 suffix: releases "<rel>" and "<rel>-infra"
// are two halves of one tenant install.
func installOf(release string) string {
	return strings.TrimSuffix(release, infraSuffix)
}

// ringOrder sequences a ring pair's uninstall: ring3 ("<rel>") first so
// app pods drain, then ring2 ("<rel>-infra") whose finalizers would
// otherwise block on live workloads.
func ringOrder(release string) int {
	if strings.HasSuffix(release, infraSuffix) {
		return 1
	}

	return 0
}

// sortActions orders deletions deterministically: by Order first (ring3
// before ring2), then by kind, namespace, release and ID so two plans of
// the same cluster are byte-identical.
func sortActions(actions []Action) {
	sort.SliceStable(actions, func(i, j int) bool {
		a, b := actions[i], actions[j]
		if a.Order != b.Order {
			return a.Order < b.Order
		}

		if a.Item.Kind != b.Item.Kind {
			return a.Item.Kind < b.Item.Kind
		}

		if a.Item.Tenant.Namespace != b.Item.Tenant.Namespace {
			return a.Item.Tenant.Namespace < b.Item.Tenant.Namespace
		}

		if a.Item.Tenant.Release != b.Item.Tenant.Release {
			return a.Item.Tenant.Release < b.Item.Tenant.Release
		}

		return a.Item.ID < b.Item.ID
	})
}

func sortKept(kept []Kept) {
	sort.SliceStable(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if a.Item.Kind != b.Item.Kind {
			return a.Item.Kind < b.Item.Kind
		}

		return a.Item.ID < b.Item.ID
	})
}

// splitTenantPath decomposes "<ns>/<rel>/rest..." into its tenant
// halves. It requires at least three segments: "<ns>/<rel>/" must be a
// container, never a record — a two-segment key is an object named
// "<ns>/<rel>", which is not a tenant prefix and is left alone.
func splitTenantPath(key string) (namespace, release string, ok bool) {
	parts := strings.SplitN(strings.TrimPrefix(key, "/"), "/", 3)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}

	return parts[0], parts[1], true
}

func topSegment(key string) string {
	seg, _, _ := strings.Cut(strings.TrimPrefix(key, "/"), "/")

	return seg
}

func roundAge(d time.Duration) time.Duration {
	return d.Round(time.Minute)
}
