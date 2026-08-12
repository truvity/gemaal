// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/gemaal/pkg/engine"
)

// TestDecommission_OverridesTheTenantsOwnProtections: keep-until and TTL
// exist to stop the janitor; the caller ending their own tenant is the
// opposite of wanting it kept. A fresh AND held tenant must still go.
func TestDecommission_OverridesTheTenantsOwnProtections(t *testing.T) {
	kube, helm, ssm := planFixture()
	// emp-jdoe/myapp is 2h old (within TTL) — hold it explicitly too.
	kube.secrets["emp-jdoe"] = []engine.ReleaseSecret{
		helmSecret("emp-jdoe", "myapp", 1, map[string]string{
			"gemaal.io/keep-until": engine.FormatKeepUntil(now.Add(24 * time.Hour)),
		}),
	}

	eng := newEngine(testConfig(t), kube, helm, ssm, nil)

	record, err := eng.Decommission(context.Background(), "emp-jdoe", "myapp", true)
	require.NoError(t, err)

	require.Len(t, record.Results, 1)
	assert.True(t, record.Results[0].Executed)
	assert.Equal(t, engine.RuleDecommission, record.Results[0].Action.Rule)
	assert.Equal(t, []string{"emp-jdoe/myapp"}, helm.uninstalled)
}

// TestDecommission_RingPairDrainsInOrder: the app half must go before
// the infra half, exactly as in a sweep — pods drain before the infra
// finalizers run.
func TestDecommission_RingPairDrainsInOrder(t *testing.T) {
	kube, helm, ssm := planFixture()
	eng := newEngine(testConfig(t), kube, helm, ssm, nil)

	record, err := eng.Decommission(context.Background(), "emp-idle", "myapp", true)
	require.NoError(t, err)

	require.Len(t, record.Results, 2)
	assert.Equal(t, []string{"emp-idle/myapp", "emp-idle/myapp-infra"}, helm.uninstalled)
}

// TestDecommission_NoSuchTenant: a target with no releases is NotFound
// territory, not an execution failure — the caller must be able to tell
// "already gone" from "failed to remove".
func TestDecommission_NoSuchTenant(t *testing.T) {
	kube, helm, ssm := planFixture()
	eng := newEngine(testConfig(t), kube, helm, ssm, nil)

	_, err := eng.Decommission(context.Background(), "emp-jdoe", "absent", true)
	require.ErrorIs(t, err, engine.ErrNoSuchTenant)
	assert.Empty(t, helm.uninstalled)
}

// TestDecommission_DryRunTouchesNothing: confirm=false records what
// would go and uninstalls nothing — the server's dry-run wins here
// exactly as it does for a sweep.
func TestDecommission_DryRunTouchesNothing(t *testing.T) {
	kube, helm, ssm := planFixture()
	eng := newEngine(testConfig(t), kube, helm, ssm, nil)

	record, err := eng.Decommission(context.Background(), "emp-idle", "myapp", false)
	require.NoError(t, err)

	assert.True(t, record.DryRun)
	require.Len(t, record.Results, 2)

	for _, result := range record.Results {
		assert.False(t, result.Executed)
	}

	assert.Empty(t, helm.uninstalled)
}

// TestDecommission_ReachStillAuthorizes: a namespace outside the
// tier-selected reach refuses even on the explicit path — decommission
// overrides the tenant's protections, never the server's.
func TestDecommission_ReachStillAuthorizes(t *testing.T) {
	kube, helm, ssm := planFixture()
	// A release in a namespace the tier selection does not include.
	kube.namespaces = append(kube.namespaces, engine.Namespace{Name: "kube-system", Tier: "untracked"})

	eng := newEngine(testConfig(t), kube, helm, ssm, nil)

	_, err := eng.Decommission(context.Background(), "kube-system", "anything", true)
	// The view only watches tier-selected namespaces, so the tenant is
	// simply not there — and nothing was uninstalled.
	require.Error(t, err)
	assert.Empty(t, helm.uninstalled)
}

// TestDecommission_RefusesALiveClaim: a running suite's claim protects
// the tenant from the explicit path too — yanking releases mid-run is
// exactly the race the claim exists to prevent. Expired claims do not
// block: dead hands release.
func TestDecommission_RefusesALiveClaim(t *testing.T) {
	kube, helm, ssm := planFixture()
	kube.leases = map[string]engine.Lease{
		"emp-idle/gemaal-claim-myapp": {Holder: "other@host#7", RenewTime: now.Add(-10 * time.Second), DurationSeconds: 90},
	}

	eng := newEngine(testConfig(t), kube, helm, ssm, nil)

	_, err := eng.Decommission(context.Background(), "emp-idle", "myapp", true)

	var claimed engine.ErrTenantClaimed
	require.ErrorAs(t, err, &claimed)
	assert.Equal(t, "other@host#7", claimed.Holder, "the refusal must name the holder")
	assert.Empty(t, helm.uninstalled)

	// The same claim, expired: the dead hand released it.
	kube.leases["emp-idle/gemaal-claim-myapp"] = engine.Lease{
		Holder: "other@host#7", RenewTime: now.Add(-10 * time.Minute), DurationSeconds: 90,
	}

	_, err = eng.Decommission(context.Background(), "emp-idle", "myapp", true)
	require.NoError(t, err)
	assert.NotEmpty(t, helm.uninstalled)
}

// TestDecommission_UnreadableLeasesFailOpen: the claim is protection for
// a running suite; a cluster where leases cannot be read must degrade to
// the pre-claim behavior, not brick the explicit path.
func TestDecommission_UnreadableLeasesFailOpen(t *testing.T) {
	kube, helm, ssm := planFixture()
	kube.leaseErr = context.DeadlineExceeded

	eng := newEngine(testConfig(t), kube, helm, ssm, nil)

	_, err := eng.Decommission(context.Background(), "emp-idle", "myapp", true)
	require.NoError(t, err)
	assert.NotEmpty(t, helm.uninstalled)
}
