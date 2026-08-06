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

func TestViewListFailuresAreFatal(t *testing.T) {
	// Release truth is not optional: any listing failure aborts the view
	// rather than presenting a world with tenants missing.
	kube := &fakeKube{namespaces: []engine.Namespace{{Name: "emp-jdoe", Tier: "employee"}}}

	_, err := newEngine(testConfig(t), kube, &fakeHelm{listErr: errors.New("rbac")}, nil, nil).View(context.Background())
	require.ErrorContains(t, err, "list helm releases")

	helm := &fakeHelm{releases: map[string][]engine.Release{
		"emp-jdoe": {{Namespace: "emp-jdoe", Name: "myapp", Updated: now}},
	}}
	kube.secretsErr = errors.New("rbac")

	_, err = newEngine(testConfig(t), kube, helm, nil, nil).View(context.Background())
	require.ErrorContains(t, err, "list release secrets")

	_, err = newEngine(testConfig(t), &fakeKube{nsErr: errors.New("down")}, helm, nil, nil).View(context.Background())
	require.ErrorContains(t, err, "list tier namespaces")

	_, err = (&engine.Engine{Config: testConfig(t)}).View(context.Background())
	require.ErrorContains(t, err, "release truth is not optional")
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
