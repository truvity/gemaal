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

func TestViewGroupsRingPairsAndMergesLedger(t *testing.T) {
	kube := &fakeKube{
		namespaces: []engine.Namespace{{Name: "emp-jdoe", Tier: "employee"}},
		secrets: map[string][]engine.ReleaseSecret{
			"emp-jdoe": {
				helmSecret("emp-jdoe", "myapp", 3, map[string]string{
					"gemaal.io/ttl":          "48h",
					"gemaal.io/execution-id": "local-1",
				}),
				helmSecret("emp-jdoe", "myapp-infra", 2, map[string]string{
					"gemaal.io/ttl":        "1h", // the app release's stamp must win
					"gemaal.io/keep-until": engine.FormatKeepUntil(now.Add(2 * time.Hour)),
				}),
			},
		},
	}
	helm := &fakeHelm{
		releases: map[string][]engine.Release{
			"emp-jdoe": {
				{Namespace: "emp-jdoe", Name: "myapp-infra", Updated: now.Add(-30 * time.Hour)},
				{Namespace: "emp-jdoe", Name: "myapp", Updated: now.Add(-2 * time.Hour)},
			},
		},
	}

	view, err := newEngine(testConfig(t), kube, helm, nil, nil).View(context.Background())
	require.NoError(t, err)

	require.Len(t, view.Tenants, 1, "the ring pair collapses to one tenant")

	tenant := view.Tenants[0]
	assert.Equal(t, engine.Tenant{Namespace: "emp-jdoe", Release: "myapp"}, tenant.Tenant)
	assert.Equal(t, "employee", tenant.Tier)

	// App before infra.
	require.Len(t, tenant.Releases, 2)
	assert.Equal(t, "myapp", tenant.Releases[0].Name)
	assert.Equal(t, "myapp-infra", tenant.Releases[1].Name)

	// Last activity: the NEWEST last-deployed across the pair.
	assert.Equal(t, now.Add(-2*time.Hour), tenant.LastActivity)

	// Ledger merge: app release wins per key; infra fills the gaps.
	assert.Equal(t, 48*time.Hour, tenant.Ledger.TTL)
	assert.Equal(t, "label", tenant.TTLSource)
	assert.Equal(t, 48*time.Hour, tenant.TTL)
	assert.Equal(t, now.Add(2*time.Hour), tenant.Ledger.KeepUntil)
	assert.Equal(t, "local-1", tenant.Ledger.ExecutionID)
	assert.Empty(t, tenant.LedgerProblems)
}

func TestViewTierDefaultTTLWhenNoLabel(t *testing.T) {
	kube := &fakeKube{
		namespaces: []engine.Namespace{{Name: "ci-truvity-bar", Tier: "ci"}},
		secrets: map[string][]engine.ReleaseSecret{
			"ci-truvity-bar": {helmSecret("ci-truvity-bar", "r1-a1", 1, nil)},
		},
	}
	helm := &fakeHelm{
		releases: map[string][]engine.Release{
			"ci-truvity-bar": {{Namespace: "ci-truvity-bar", Name: "r1-a1", Updated: now.Add(-time.Hour)}},
		},
	}

	view, err := newEngine(testConfig(t), kube, helm, nil, nil).View(context.Background())
	require.NoError(t, err)

	require.Len(t, view.Tenants, 1)
	assert.Equal(t, 24*time.Hour, view.Tenants[0].TTL)
	assert.Equal(t, "tier-default", view.Tenants[0].TTLSource)
}

func TestViewUnconfiguredTierIsOutOfReach(t *testing.T) {
	// A namespace whose tier is not in the config is skipped even when
	// the kube client returns it: expansion can only narrow the reach.
	kube := &fakeKube{
		namespaces: []engine.Namespace{
			{Name: "biz-payments", Tier: "primary"},
			{Name: "emp-jdoe", Tier: "employee"},
		},
		secrets: map[string][]engine.ReleaseSecret{},
	}
	helm := &fakeHelm{releases: map[string][]engine.Release{
		"biz-payments": {{Namespace: "biz-payments", Name: "payments", Updated: now.Add(-100 * time.Hour)}},
		"emp-jdoe":     {{Namespace: "emp-jdoe", Name: "myapp", Updated: now.Add(-time.Hour)}},
	}}

	view, err := newEngine(testConfig(t), kube, helm, nil, nil).View(context.Background())
	require.NoError(t, err)

	require.Len(t, view.Namespaces, 1)
	assert.Equal(t, "emp-jdoe", view.Namespaces[0].Name)
	require.Len(t, view.Tenants, 1)
	assert.Equal(t, "emp-jdoe", view.Tenants[0].Tenant.Namespace)
}

func TestViewUnparseableLedgerIsAProblemNotAGuess(t *testing.T) {
	kube := &fakeKube{
		namespaces: []engine.Namespace{{Name: "emp-jdoe", Tier: "employee"}},
		secrets: map[string][]engine.ReleaseSecret{
			"emp-jdoe": {
				helmSecret("emp-jdoe", "myapp", 1, map[string]string{
					"gemaal.io/ttl":        "soon",
					"gemaal.io/keep-until": "tomorrow",
				}),
			},
		},
	}
	helm := &fakeHelm{releases: map[string][]engine.Release{
		"emp-jdoe": {{Namespace: "emp-jdoe", Name: "myapp", Updated: now.Add(-1000 * time.Hour)}},
	}}

	view, err := newEngine(testConfig(t), kube, helm, nil, nil).View(context.Background())
	require.NoError(t, err)

	require.Len(t, view.Tenants, 1)
	tenant := view.Tenants[0]
	assert.Len(t, tenant.LedgerProblems, 2)
	assert.True(t, tenant.Held(now), "a broken stamp holds the tenant")
	assert.False(t, tenant.Expired(now))
}

func TestViewListFailuresDegradeRatherThanBlind(t *testing.T) {
	// Release truth is not optional — but "not optional" used to mean the
	// whole view aborted, which presented the operator with NOTHING rather
	// than a world with one namespace missing. Both are honest; only one is
	// useful. The failure is now named per namespace, and the artifact
	// sweeps refuse to judge orphans while any namespace is unread
	// (TestPlanRefusesArtifactSweepsOnIncompleteView).
	kube := &fakeKube{namespaces: []engine.Namespace{{Name: "emp-jdoe", Tier: "employee"}}}

	view, err := newEngine(testConfig(t), kube, &fakeHelm{listErr: errors.New("rbac")}, nil, nil).View(context.Background())
	require.NoError(t, err)
	require.Len(t, view.Unreadable, 1)
	assert.Equal(t, "emp-jdoe", view.Unreadable[0].Namespace)
	assert.Contains(t, view.Unreadable[0].Reason, "list helm releases")
	assert.Empty(t, view.Tenants, "an unread namespace contributes no tenants")

	// The ledger lives in the release secrets, so losing them loses TTL and
	// keep-until — the same category of blindness, reported the same way.
	helm := &fakeHelm{releases: map[string][]engine.Release{
		"emp-jdoe": {{Namespace: "emp-jdoe", Name: "myapp", Updated: now}},
	}}
	kube.secretsErr = errors.New("rbac")

	view, err = newEngine(testConfig(t), kube, helm, nil, nil).View(context.Background())
	require.NoError(t, err)
	require.Len(t, view.Unreadable, 1)
	assert.Contains(t, view.Unreadable[0].Reason, "list release secrets")
	assert.False(t, view.Complete())
}

func TestStampKeepUntilStampsBothRingHalves(t *testing.T) {
	kube := &fakeKube{
		namespaces: []engine.Namespace{{Name: "emp-jdoe", Tier: "employee"}},
		secrets: map[string][]engine.ReleaseSecret{
			"emp-jdoe": {
				helmSecret("emp-jdoe", "myapp", 4, nil),
				helmSecret("emp-jdoe", "myapp-infra", 2, nil),
				helmSecret("emp-jdoe", "other", 1, nil),
			},
		},
	}

	eng := newEngine(testConfig(t), kube, &fakeHelm{}, nil, nil)

	until := now.Add(8 * time.Hour)
	require.NoError(t, eng.StampKeepUntil(context.Background(), engine.Tenant{Namespace: "emp-jdoe", Release: "myapp"}, until))

	require.Len(t, kube.labeled, 2, "both ring halves carry the hold; the foreign release is untouched")
	assert.Contains(t, kube.labeled[0], "sh.helm.release.v1.myapp.v4")
	assert.Contains(t, kube.labeled[0], "gemaal.io/keep-until="+engine.FormatKeepUntil(until))
	assert.Contains(t, kube.labeled[1], "sh.helm.release.v1.myapp-infra.v2")
}

func TestStampKeepUntilRefusesUnknownTenant(t *testing.T) {
	kube := &fakeKube{secrets: map[string][]engine.ReleaseSecret{}}

	eng := newEngine(testConfig(t), kube, &fakeHelm{}, nil, nil)

	err := eng.StampKeepUntil(context.Background(), engine.Tenant{Namespace: "emp-jdoe", Release: "ghost"}, now)
	require.ErrorContains(t, err, "no release secrets")
}

// One namespace must not cost the other 27. Observed on devel as a killed
// `helm list` in emp-ijovi surfacing to the operator as "could not build
// the tenant view" for every namespace at once.
func TestViewSurvivesOneUnreadableNamespace(t *testing.T) {
	kube, helm, _ := planFixture()
	helm.listErrs = map[string]error{
		"emp-jdoe": errors.New("list helm releases in emp-jdoe: signal: killed: "),
	}

	view, err := newEngine(testConfig(t), kube, helm, nil, nil).View(context.Background())
	require.NoError(t, err, "one bad namespace is not a bad view")

	require.False(t, view.Complete(), "a view missing a namespace is not complete")
	require.Len(t, view.Unreadable, 1)
	assert.Equal(t, "emp-jdoe", view.Unreadable[0].Namespace)
	assert.Contains(t, view.Unreadable[0].Reason, "signal: killed")

	// The readable namespaces are still there — the whole point.
	var names []string
	for _, tenant := range view.Tenants {
		names = append(names, tenant.Tenant.Namespace)
	}

	assert.Contains(t, names, "emp-idle")
	assert.Contains(t, names, "ci-truvity-bar")
	assert.NotContains(t, names, "emp-jdoe", "its releases were never read")
}

// The safety half. emp-jdoe's SSM parameter is spared only because myapp is
// observed live; if the namespace cannot be read, that evidence is missing
// and the parameter looks like an orphan past grace. It must not be touched.
func TestPlanRefusesArtifactSweepsOnIncompleteView(t *testing.T) {
	kube, helm, ssm := planFixture()

	// Control: with a whole view the live tenant's parameter is kept and
	// the genuine orphan is deleted.
	whole, err := newEngine(testConfig(t), kube, helm, ssm, nil).Plan(context.Background(), engine.Narrow{})
	require.NoError(t, err)
	require.NotEmpty(t, deletedTargets(whole), "fixture must delete something, or this proves nothing")
	assert.NotContains(t, deletedTargets(whole), "/test/emp-jdoe/myapp/db-url")

	// Now break exactly the namespace whose liveness spares that parameter.
	helm.listErrs = map[string]error{"emp-jdoe": errors.New("signal: killed")}

	partial, err := newEngine(testConfig(t), kube, helm, ssm, nil).Plan(context.Background(), engine.Narrow{})
	require.NoError(t, err)

	assert.NotContains(t, deletedTargets(partial), "/test/emp-jdoe/myapp/db-url",
		"an unread namespace must never make a live tenant's artifacts look orphaned")

	for _, target := range deletedTargets(partial) {
		assert.NotContains(t, target, "/test/", "no artifact is judged while the view is partial")
	}

	require.NotEmpty(t, partial.Problems, "the skipped sweep must be visible in the plan")
	assert.Contains(t, partial.Problems[0].Err, "artifact sweeps skipped")
}

// A caller that walks away kills every child helm process. That is one dead
// request, not 28 broken namespaces, and inventing the latter would look
// exactly like a real outage.
func TestViewReportsCancellationRatherThanBlamingNamespaces(t *testing.T) {
	kube, helm, _ := planFixture()
	helm.blockFor = map[string]bool{"ci-truvity-bar": true}

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	view, err := newEngine(testConfig(t), kube, helm, nil, nil).View(ctx)

	require.Error(t, err, "an abandoned view is an error, not a partial answer")
	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, view)
}
