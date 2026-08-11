package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/gemaal/pkg/engine"
)

// planFixture is a cluster with one fresh employee install, one expired
// employee install (ring pair), one expired CI install, and SSM state
// for a live tenant plus an old orphan.
func planFixture() (*fakeKube, *fakeHelm, *fakeSSM) {
	kube := &fakeKube{
		namespaces: []engine.Namespace{
			{Name: "ci-truvity-bar", Tier: "ci"},
			{Name: "emp-fresh", Tier: "employee"},
			{Name: "emp-gone", Tier: "employee"},
			{Name: "emp-idle", Tier: "employee"},
			{Name: "emp-jdoe", Tier: "employee"},
		},
		secrets: map[string][]engine.ReleaseSecret{
			"emp-jdoe":       {helmSecret("emp-jdoe", "myapp", 1, nil)},
			"emp-idle":       {helmSecret("emp-idle", "myapp", 1, nil), helmSecret("emp-idle", "myapp-infra", 1, nil)},
			"ci-truvity-bar": {helmSecret("ci-truvity-bar", "r1-a1", 1, nil)},
		},
	}

	helm := &fakeHelm{
		releases: map[string][]engine.Release{
			"emp-jdoe": {{Namespace: "emp-jdoe", Name: "myapp", Updated: now.Add(-2 * time.Hour)}},
			"emp-idle": {
				{Namespace: "emp-idle", Name: "myapp-infra", Updated: now.Add(-40 * time.Hour)},
				{Namespace: "emp-idle", Name: "myapp", Updated: now.Add(-39 * time.Hour)},
			},
			"ci-truvity-bar": {{Namespace: "ci-truvity-bar", Name: "r1-a1", Updated: now.Add(-30 * time.Hour)}},
		},
		uninstallErr: map[string]error{},
	}

	ssm := &fakeSSM{
		parameters: []engine.Parameter{
			// Live tenant's state: kept, release-live.
			{Name: "/test/emp-jdoe/myapp/db-url", LastModified: now.Add(-90 * time.Hour)},
			// Orphan past grace: deleted.
			{Name: "/test/emp-gone/myapp/db-url", LastModified: now.Add(-72 * time.Hour)},
			{Name: "/test/emp-gone/myapp/api-key", LastModified: now.Add(-72 * time.Hour)},
			// Orphan within grace: kept.
			{Name: "/test/emp-fresh/myapp/db-url", LastModified: now.Add(-time.Hour)},
			// Not a tenant shape: kept.
			{Name: "/test/loose-parameter", LastModified: now.Add(-500 * time.Hour)},
			// Tenant namespace outside the tier-selected reach: kept.
			{Name: "/test/emp-departed/myapp/db-url", LastModified: now.Add(-500 * time.Hour)},
		},
	}

	return kube, helm, ssm
}

func TestPlanUniformTTLAndOrphanRules(t *testing.T) {
	kube, helm, ssm := planFixture()

	plan, err := newEngine(testConfig(t), kube, helm, ssm, nil).Plan(context.Background(), engine.Narrow{})
	require.NoError(t, err)

	deletes := map[string]engine.Rule{}
	for _, a := range plan.Delete {
		deletes[string(a.Item.Kind)+" "+a.Item.ID] = a.Rule
	}

	assert.Equal(t, map[string]engine.Rule{
		// The uniform TTL: an idle EMPLOYEE standing install expires like
		// any other tenant — no special case.
		"helm-release emp-idle/myapp":       engine.RuleTTLExpired,
		"helm-release emp-idle/myapp-infra": engine.RuleTTLExpired,
		"helm-release ci-truvity-bar/r1-a1": engine.RuleTTLExpired,
		"ssm-prefix /test/emp-gone/myapp/":  engine.RuleOrphanGrace,
	}, deletes)

	kept := map[string]engine.KeepReason{}
	for _, k := range plan.Keep {
		kept[string(k.Item.Kind)+" "+k.Item.ID] = k.Reason
	}

	assert.Equal(t, map[string]engine.KeepReason{
		"helm-release emp-jdoe/myapp":          engine.KeepWithinTTL,
		"ssm-prefix /test/emp-jdoe/myapp/":     engine.KeepReleaseLive,
		"ssm-prefix /test/emp-fresh/myapp/":    engine.KeepWithinGrace,
		"ssm-prefix /test/loose-parameter/":    engine.KeepUnknownTenantShape,
		"ssm-prefix /test/emp-departed/myapp/": engine.KeepOutOfReach,
	}, kept)
}

func TestPlanRingOrderAppBeforeInfra(t *testing.T) {
	kube, helm, ssm := planFixture()

	plan, err := newEngine(testConfig(t), kube, helm, ssm, nil).Plan(context.Background(), engine.Narrow{Namespace: "emp-idle"})
	require.NoError(t, err)

	require.Len(t, plan.Delete, 2)
	assert.Equal(t, "emp-idle/myapp", plan.Delete[0].Item.ID, "the app uninstalls first")
	assert.Equal(t, "emp-idle/myapp-infra", plan.Delete[1].Item.ID, "infra second — the app never outlives it")
}

func TestPlanKeepUntilOverridesTTL(t *testing.T) {
	kube, helm, ssm := planFixture()

	// The idle tenant is way past TTL, but a keep-until in the future
	// holds it — keep-until precedence is the human override's contract.
	kube.secrets["emp-idle"] = []engine.ReleaseSecret{
		helmSecret("emp-idle", "myapp", 2, map[string]string{
			"gemaal.io/keep-until": engine.FormatKeepUntil(now.Add(time.Hour)),
		}),
		helmSecret("emp-idle", "myapp-infra", 1, nil),
	}

	plan, err := newEngine(testConfig(t), kube, helm, ssm, nil).Plan(context.Background(), engine.Narrow{Namespace: "emp-idle"})
	require.NoError(t, err)

	assert.Empty(t, plan.Delete)

	reasons := map[engine.KeepReason]int{}
	for _, k := range plan.Keep {
		reasons[k.Reason]++
	}

	assert.Equal(t, 2, reasons[engine.KeepUntilHolds], "both ring halves are spared by the hold")
}

func TestPlanExpiredKeepUntilNoLongerHolds(t *testing.T) {
	kube, helm, ssm := planFixture()

	kube.secrets["emp-idle"] = []engine.ReleaseSecret{
		helmSecret("emp-idle", "myapp", 2, map[string]string{
			"gemaal.io/keep-until": engine.FormatKeepUntil(now.Add(-time.Minute)),
		}),
		helmSecret("emp-idle", "myapp-infra", 1, nil),
	}

	plan, err := newEngine(testConfig(t), kube, helm, ssm, nil).Plan(context.Background(), engine.Narrow{Namespace: "emp-idle"})
	require.NoError(t, err)

	assert.Len(t, plan.Delete, 2, "a passed keep-until protects nothing")
}

func TestPlanTTLLabelBeatsTierDefault(t *testing.T) {
	kube, helm, ssm := planFixture()

	// 39h idle, tier default 14h — but the tenant's own label says 72h.
	kube.secrets["emp-idle"] = []engine.ReleaseSecret{
		helmSecret("emp-idle", "myapp", 2, map[string]string{"gemaal.io/ttl": "72h"}),
		helmSecret("emp-idle", "myapp-infra", 1, nil),
	}

	plan, err := newEngine(testConfig(t), kube, helm, ssm, nil).Plan(context.Background(), engine.Narrow{Namespace: "emp-idle"})
	require.NoError(t, err)

	assert.Empty(t, plan.Delete)
}

func TestPlanLastActivityIsTheNewestRingHalf(t *testing.T) {
	kube, helm, ssm := planFixture()

	// The infra half was upgraded just now: activity on either half is
	// activity on the tenant.
	helm.releases["emp-idle"] = []engine.Release{
		{Namespace: "emp-idle", Name: "myapp", Updated: now.Add(-39 * time.Hour)},
		{Namespace: "emp-idle", Name: "myapp-infra", Updated: now.Add(-time.Hour)},
	}

	plan, err := newEngine(testConfig(t), kube, helm, ssm, nil).Plan(context.Background(), engine.Narrow{Namespace: "emp-idle"})
	require.NoError(t, err)

	assert.Empty(t, plan.Delete)
}

func TestPlanUnparseableLedgerHoldsAndReports(t *testing.T) {
	kube, helm, ssm := planFixture()

	kube.secrets["emp-idle"] = []engine.ReleaseSecret{
		helmSecret("emp-idle", "myapp", 2, map[string]string{"gemaal.io/keep-until": "not-a-time"}),
		helmSecret("emp-idle", "myapp-infra", 1, nil),
	}

	plan, err := newEngine(testConfig(t), kube, helm, ssm, nil).Plan(context.Background(), engine.Narrow{Namespace: "emp-idle"})
	require.NoError(t, err)

	assert.Empty(t, plan.Delete, "never delete on garbage input")

	require.NotEmpty(t, plan.Problems)
	assert.Contains(t, plan.Problems[0].Err, "keep-until")
}

func TestPlanUnparseableTimestampHoldsReleaseNotTheTick(t *testing.T) {
	kube, helm, ssm := planFixture()

	// The devel incident (2026-08-06): helm v4 stamped "updated" as
	// "... +0200 +0200" and the parser refused it — and the whole
	// housekeeping tick aborted. The failure must stay with the ONE
	// release: its tenant is held with the problem recorded, and every
	// other tenant in the estate is planned normally.
	kube.namespaces = append(kube.namespaces, engine.Namespace{Name: "ci-truvity-shadow", Tier: "ci"})
	kube.secrets["ci-truvity-shadow"] = []engine.ReleaseSecret{
		helmSecret("ci-truvity-shadow", "shadow-proof", 1, nil),
	}
	helm.releases["ci-truvity-shadow"] = []engine.Release{{
		Namespace:      "ci-truvity-shadow",
		Name:           "shadow-proof",
		UpdatedProblem: `unrecognized helm updated timestamp "2026-08-06 14:26:40.885437057 +0200 +0200"`,
	}}

	plan, err := newEngine(testConfig(t), kube, helm, ssm, nil).Plan(context.Background(), engine.Narrow{})
	require.NoError(t, err, "one broken release must not stall planning for the estate")

	// The broken release is HELD, with the reason on the keep record.
	kept := map[string]engine.KeepReason{}
	for _, k := range plan.Keep {
		kept[k.Item.ID] = k.Reason
	}

	assert.Equal(t, engine.KeepLedgerUnparseable, kept["ci-truvity-shadow/shadow-proof"])

	// The broken stamp is surfaced as a problem an operator can read.
	problems := map[string]string{}
	for _, p := range plan.Problems {
		problems[p.Target] = p.Err
	}

	assert.Contains(t, problems["ci-truvity-shadow/shadow-proof"], "unrecognized helm updated timestamp")

	// The rest of the estate is planned normally: the expired tenants
	// are still deleted, exactly as in the baseline fixture.
	deletes := map[string]engine.Rule{}
	for _, a := range plan.Delete {
		deletes[string(a.Item.Kind)+" "+a.Item.ID] = a.Rule
	}

	assert.Equal(t, map[string]engine.Rule{
		"helm-release emp-idle/myapp":       engine.RuleTTLExpired,
		"helm-release emp-idle/myapp-infra": engine.RuleTTLExpired,
		"helm-release ci-truvity-bar/r1-a1": engine.RuleTTLExpired,
		"ssm-prefix /test/emp-gone/myapp/":  engine.RuleOrphanGrace,
	}, deletes)
}

func TestPlanArtifactsJudgedAgainstFullLiveSetWhenNarrowed(t *testing.T) {
	kube, helm, ssm := planFixture()

	// Narrowed to emp-gone's tenant: its orphan is planned; emp-jdoe's
	// live state is out of the narrow but MUST NOT become an orphan.
	plan, err := newEngine(testConfig(t), kube, helm, ssm, nil).Plan(context.Background(),
		engine.Narrow{Namespace: "emp-gone", Release: "myapp"})
	require.NoError(t, err)

	require.Len(t, plan.Delete, 1)
	assert.Equal(t, "/test/emp-gone/myapp/", plan.Delete[0].Item.ID)
}

func TestPlanSSMWithoutClientIsAProblem(t *testing.T) {
	kube, helm, _ := planFixture()

	plan, err := newEngine(testConfig(t), kube, helm, nil, nil).Plan(context.Background(), engine.Narrow{})
	require.NoError(t, err)

	require.NotEmpty(t, plan.Problems)
	assert.Equal(t, "/test/", plan.Problems[0].Target)
	assert.Contains(t, plan.Problems[0].Err, "no ssm client")
}

func TestPlanS3StubReportsItself(t *testing.T) {
	cfg := testConfig(t)
	cfg.AllowList.S3Buckets = []string{"truvity-devel-test"}

	kube, helm, ssm := planFixture()

	plan, err := newEngine(cfg, kube, helm, ssm, nil).Plan(context.Background(), engine.Narrow{})
	require.NoError(t, err)

	found := false

	for _, p := range plan.Problems {
		if p.Target == "s3://truvity-devel-test" {
			assert.Contains(t, p.Err, "stubbed off")

			found = true
		}
	}

	assert.True(t, found, "a skipped S3 target says so instead of staying silent")
}

func TestPlanS3SweepWithClient(t *testing.T) {
	cfg := testConfig(t)
	cfg.AllowList.S3Buckets = []string{"truvity-devel-test"}

	kube, helm, ssm := planFixture()

	s3 := &fakeS3{objects: map[string][]engine.Object{
		"truvity-devel-test": {
			{Key: "emp-jdoe/myapp/state.json", LastModified: now.Add(-90 * time.Hour)},
			{Key: "emp-gone/myapp/state.json", LastModified: now.Add(-90 * time.Hour)},
		},
	}}

	plan, err := newEngine(cfg, kube, helm, ssm, s3).Plan(context.Background(), engine.Narrow{})
	require.NoError(t, err)

	var s3Deletes []string

	for _, a := range plan.Delete {
		if a.Item.Kind == engine.KindS3Prefix {
			s3Deletes = append(s3Deletes, a.Item.ID)
		}
	}

	assert.Equal(t, []string{"s3://truvity-devel-test/emp-gone/myapp/"}, s3Deletes)
}

// deletedTargets lists what a plan would destroy, by target identity.
func deletedTargets(plan *engine.Plan) []string {
	targets := make([]string, 0, len(plan.Delete))
	for i := range plan.Delete {
		targets = append(targets, plan.Delete[i].Item.ID)
	}

	return targets
}
