package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/gemaal/pkg/engine"
)

func TestApplyShadowModePerformsNothing(t *testing.T) {
	kube, helm, ssm := planFixture()
	eng := newEngine(testConfig(t), kube, helm, ssm, nil)

	plan, err := eng.Plan(context.Background(), engine.Narrow{})
	require.NoError(t, err)
	require.NotEmpty(t, plan.Delete, "the fixture has garbage — otherwise this test proves nothing")

	record, err := eng.Apply(context.Background(), plan, false, "loop")
	require.NoError(t, err)

	// Every action recorded, none executed, no client touched.
	assert.True(t, record.DryRun)
	assert.Len(t, record.Results, len(plan.Delete))

	for _, result := range record.Results {
		assert.False(t, result.Executed)
		assert.Zero(t, result.Deleted)
	}

	assert.Empty(t, helm.uninstalled, "shadow mode must not uninstall")
	assert.Empty(t, ssm.deleted, "shadow mode must not delete parameters")
}

func TestApplyConfirmedExecutesInRingOrder(t *testing.T) {
	kube, helm, ssm := planFixture()
	eng := newEngine(testConfig(t), kube, helm, ssm, nil)

	plan, err := eng.Plan(context.Background(), engine.Narrow{})
	require.NoError(t, err)

	record, err := eng.Apply(context.Background(), plan, true, "rpc")
	require.NoError(t, err)
	assert.False(t, record.DryRun)

	// Ring order: every order-0 (app) release before any order-1 (infra).
	assert.Equal(t, []string{
		"ci-truvity-bar/r1-a1",
		"emp-idle/myapp",
		"emp-idle/myapp-infra",
	}, helm.uninstalled)

	require.Len(t, ssm.deleted, 1)
	assert.ElementsMatch(t, []string{"/test/emp-gone/myapp/api-key", "/test/emp-gone/myapp/db-url"}, ssm.deleted[0])
}

func TestApplyRefusesOutOfReachBeforeAnythingDeletes(t *testing.T) {
	kube, helm, ssm := planFixture()
	eng := newEngine(testConfig(t), kube, helm, ssm, nil)

	plan, err := eng.Plan(context.Background(), engine.Narrow{})
	require.NoError(t, err)

	// A tampered (or stale-config) plan: an action outside the
	// tier-selected reach.
	plan.Delete = append(plan.Delete, engine.Action{
		Item: engine.Item{
			Kind:   engine.KindHelmRelease,
			Tenant: engine.Tenant{Namespace: "kube-system", Release: "coredns"},
			ID:     "kube-system/coredns",
		},
		Rule: engine.RuleTTLExpired,
	})

	_, err = eng.Apply(context.Background(), plan, true, "rpc")
	require.ErrorIs(t, err, engine.ErrOutOfReach)

	assert.Empty(t, helm.uninstalled, "one unauthorized action fails the whole plan BEFORE any deletion")
	assert.Empty(t, ssm.deleted)
}

func TestApplyRefusesSSMNamesOutsideTheSubtree(t *testing.T) {
	kube, helm, ssm := planFixture()
	eng := newEngine(testConfig(t), kube, helm, ssm, nil)

	plan, err := eng.Plan(context.Background(), engine.Narrow{Namespace: "emp-gone"})
	require.NoError(t, err)
	require.Len(t, plan.Delete, 1)

	// Smuggle a foreign parameter into the action's name list.
	plan.Delete[0].Item.Names = append(plan.Delete[0].Item.Names, "/test/emp-jdoe/myapp/db-url")

	_, err = eng.Apply(context.Background(), plan, true, "rpc")
	require.ErrorIs(t, err, engine.ErrOutOfReach)
	assert.Empty(t, ssm.deleted)
}

func TestApplyRefusesForeignSSMRoot(t *testing.T) {
	eng := newEngine(testConfig(t), &fakeKube{}, &fakeHelm{}, &fakeSSM{}, nil)

	plan := &engine.Plan{
		Namespaces: []string{"emp-gone"},
		Delete: []engine.Action{{
			Item: engine.Item{
				Kind:   engine.KindSSMPrefix,
				Tenant: engine.Tenant{Namespace: "emp-gone", Release: "myapp"},
				ID:     "/prod/emp-gone/myapp/",
				Names:  []string{"/prod/emp-gone/myapp/db-url"},
			},
			Rule: engine.RuleOrphanGrace,
		}},
	}

	_, err := eng.Apply(context.Background(), plan, true, "rpc")
	require.ErrorIs(t, err, engine.ErrOutOfReach)
}

func TestApplyRecordsPartialFailures(t *testing.T) {
	kube, helm, ssm := planFixture()
	helm.uninstallErr["emp-idle/myapp"] = errors.New("wedged finalizer")

	eng := newEngine(testConfig(t), kube, helm, ssm, nil)

	plan, err := eng.Plan(context.Background(), engine.Narrow{})
	require.NoError(t, err)

	record, err := eng.Apply(context.Background(), plan, true, "rpc")
	require.ErrorContains(t, err, "wedged finalizer")

	failed := 0

	for _, result := range record.Results {
		if result.Err != "" {
			failed++

			assert.False(t, result.Executed)
		}
	}

	assert.Equal(t, 1, failed, "the failure is recorded; the rest of the plan still ran")
	assert.Contains(t, helm.uninstalled, "emp-idle/myapp-infra")
}

// TestHistoryRecord_CollapsesQuietRuns: a day of no-op loop ticks must
// be ONE line refreshing its timestamp, not a buffer full of silence —
// while a real sweep breaks the run so the timeline stays honest.
func TestHistoryRecord_CollapsesQuietRuns(t *testing.T) {
	h := engine.NewHistory(10)

	t1 := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	t3 := t2.Add(time.Minute)

	h.Record(engine.SweepRecord{At: t1, Source: "loop", Kept: 3})
	h.Record(engine.SweepRecord{At: t2, Source: "loop", Kept: 4})

	records := h.List()
	require.Len(t, records, 1, "consecutive quiet ticks collapse")
	assert.Equal(t, 2, records[0].QuietTicks)
	assert.Equal(t, t2, records[0].At, "the newest tick's timestamp proves the loop is alive")
	assert.Equal(t, 4, records[0].Kept, "kept follows the latest pass")

	// A real sweep breaks the quiet run...
	h.Record(engine.SweepRecord{At: t3, Source: "loop",
		Results: []engine.ActionResult{{Executed: true}}})
	// ...and the next quiet tick starts a NEW aggregate instead of
	// reaching across it.
	h.Record(engine.SweepRecord{At: t3.Add(time.Minute), Source: "loop"})

	records = h.List()
	require.Len(t, records, 3)
	assert.Equal(t, 1, records[0].QuietTicks, "fresh aggregate after the real sweep")
	assert.Zero(t, records[1].QuietTicks, "the real sweep is a normal record")
	assert.Equal(t, 2, records[2].QuietTicks, "the earlier run stays closed")
}

// TestHistoryRecord_ProblemsAreNotQuiet: an enumeration problem is a
// finding even with nothing planned — it must not vanish into a quiet
// aggregate.
func TestHistoryRecord_ProblemsAreNotQuiet(t *testing.T) {
	h := engine.NewHistory(10)

	h.Record(engine.SweepRecord{Source: "loop",
		Problems: []engine.TargetProblem{{}}})
	h.Record(engine.SweepRecord{Source: "loop"})

	records := h.List()
	require.Len(t, records, 2)
	assert.Zero(t, records[1].QuietTicks, "the problem record is a normal one")
	assert.Equal(t, 1, records[0].QuietTicks)
}

func TestHistoryBoundsAndOrders(t *testing.T) {
	h := engine.NewHistory(2)

	h.Add(engine.SweepRecord{Source: "a"})
	h.Add(engine.SweepRecord{Source: "b"})
	h.Add(engine.SweepRecord{Source: "c"})

	records := h.List()
	require.Len(t, records, 2)
	assert.Equal(t, "c", records[0].Source, "newest first")
	assert.Equal(t, "b", records[1].Source)
}
