package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func leaseJSON(holder string, renewedAgo time.Duration, durationSeconds int) string {
	var l leaseObject
	l.APIVersion = "coordination.k8s.io/v1"
	l.Kind = "Lease"
	l.Metadata.Name = "gemaal-claim-myapp"
	l.Metadata.Namespace = "emp-jdoe"
	l.Metadata.ResourceVersion = "42"
	l.Spec.HolderIdentity = holder
	l.Spec.LeaseDurationSeconds = durationSeconds
	l.Spec.RenewTime = time.Now().UTC().Add(-renewedAgo).Format(leaseTimeLayout)

	data, _ := json.Marshal(l)

	return string(data)
}

func claimFixture() (*Cluster, Tenant, *stubRunner) {
	s := &stubRunner{}

	return &Cluster{Runner: s},
		Tenant{Namespace: "emp-jdoe", Release: "myapp"}, s
}

// TestClaimTenant_FreshClaim: no lease → create → protected.
func TestClaimTenant_FreshClaim(t *testing.T) {
	c, tenant, s := claimFixture()
	// The REAL runner yields empty output + nil error for an absent
	// lease (--ignore-not-found); kubectl's NotFound text only ever goes
	// to the streamed stderr. A stub returning that text in the error
	// encoded exactly the wrong assumption that shipped a claim which
	// never engaged.
	s.on("kubectl get lease gemaal-claim-myapp", "", nil)

	claim, err := c.ClaimTenant(context.Background(), tenant, "me@host#1")
	if err != nil {
		t.Fatalf("fresh claim must succeed, got %v", err)
	}

	if claim == nil {
		t.Fatal("fresh claim must return a live claim, not fail-open nil")
	}

	t.Cleanup(func() { claim.cancel(); <-claim.done })

	var created bool

	for _, call := range s.joined() {
		if strings.Contains(call, "kubectl create -f") {
			created = true
		}
	}

	if !created {
		t.Fatalf("expected a kubectl create, got %v", s.joined())
	}
}

// TestClaimTenant_HeldByAnother: a LIVE foreign lease is a hard stop that
// NAMES the holder — that name is the whole difference between "deploy
// failed" and "another agent is using this release".
func TestClaimTenant_HeldByAnother(t *testing.T) {
	c, tenant, s := claimFixture()
	s.on("kubectl get lease", leaseJSON("other@host#7", 5*time.Second, 90), nil)

	_, err := c.ClaimTenant(context.Background(), tenant, "me@host#1")

	var held ErrTenantHeld
	if !errors.As(err, &held) {
		t.Fatalf("want ErrTenantHeld, got %v", err)
	}

	if held.Holder != "other@host#7" {
		t.Fatalf("the error must name the holder, got %q", held.Holder)
	}

	if !strings.Contains(err.Error(), EnvRelease) {
		t.Fatalf("the error must tell the operator about %s, got %q", EnvRelease, err)
	}
}

// TestClaimTenant_ExpiredTakeover: an abandoned claim frees itself — the
// dead-hand property. Takeover must go through `kubectl replace`, whose
// resourceVersion carries the compare-and-swap.
func TestClaimTenant_ExpiredTakeover(t *testing.T) {
	c, tenant, s := claimFixture()
	s.on("kubectl get lease", leaseJSON("dead@host#9", 10*time.Minute, 90), nil)

	claim, err := c.ClaimTenant(context.Background(), tenant, "me@host#1")
	if err != nil {
		t.Fatalf("expired takeover must succeed, got %v", err)
	}

	t.Cleanup(func() { claim.cancel(); <-claim.done })

	var replaced bool

	for _, call := range s.joined() {
		if strings.Contains(call, "kubectl replace -f") {
			replaced = true
		}
	}

	if !replaced {
		t.Fatalf("takeover must use kubectl replace (CAS via resourceVersion), got %v", s.joined())
	}
}

// TestClaimTenant_UnavailableFailsOpen: lease-API trouble degrades to the
// pre-claim status quo (nil claim, nil error) instead of blocking every
// suite on coordination infrastructure it can live without.
func TestClaimTenant_UnavailableFailsOpen(t *testing.T) {
	c, tenant, s := claimFixture()
	s.on("kubectl get lease", "", errors.New("the server is currently unable to handle the request"))

	claim, err := c.ClaimTenant(context.Background(), tenant, "me@host#1")
	if err != nil {
		t.Fatalf("unavailable must fail open, got error %v", err)
	}

	if claim != nil {
		t.Fatal("unavailable must return a nil claim, not a live one")
	}
}

// TestClaimRelease_OnlyDeletesOwn: releasing checks the holder first — a
// lease stolen after our expiry belongs to its new holder, and deleting
// it would hand OUR bug to THEIR suite.
func TestClaimRelease_OnlyDeletesOwn(t *testing.T) {
	c, tenant, s := claimFixture()
	s.on("kubectl get lease", "", nil)

	claim, err := c.ClaimTenant(context.Background(), tenant, "me@host#1")
	if err != nil || claim == nil {
		t.Fatalf("claim: %v", err)
	}

	// By release time the lease belongs to someone else.
	s.on("kubectl get lease", leaseJSON("thief@host#3", time.Second, 90), nil)

	claim.Release(context.Background())

	for _, call := range s.joined() {
		if strings.Contains(call, "kubectl delete lease") {
			t.Fatalf("must not delete a lease held by another, got %v", s.joined())
		}
	}
}

// TestClaimWithAllocation_WalksDerivedLanes: a held DERIVED release
// allocates the next lane and re-exports it, so the suite runs against
// the install that actually claimed.
func TestClaimWithAllocation_WalksDerivedLanes(t *testing.T) {
	// Shield the process env: Export writes the GEMAAL_* trio, and the
	// CI-derivation variables disable allocation by design — a CI runner
	// executing THIS test must not veto the scenario it is testing.
	t.Setenv(EnvNamespace, "")
	t.Setenv(EnvRelease, "")
	t.Setenv(EnvKubecontext, "")
	t.Setenv(EnvCIRunNumber, "")
	t.Setenv(EnvCIRunAttempt, "")

	s := &stubRunner{}
	// Base lane held LIVE; lane -2 absent (empty output, nil error).
	s.on("kubectl get lease gemaal-claim-myapp ", leaseJSON("other@host#7", 5*time.Second, 90), nil)
	s.on("kubectl get lease gemaal-claim-myapp-2", "", nil)

	suite := Suite{Options: Options{App: "myapp"}}
	cluster := &Cluster{Runner: s}
	tenant := Tenant{Namespace: "emp-jdoe", Release: "myapp"}

	claim, got, err := suite.claimWithAllocation(context.Background(), cluster, tenant)
	require.NoError(t, err)
	require.NotNil(t, claim, "lane -2 must be claimed, not failed")

	t.Cleanup(func() { claim.cancel(); <-claim.done })

	assert.Equal(t, "myapp-2", got.Release)
	assert.Equal(t, "myapp-2", os.Getenv(EnvRelease),
		"the allocated lane must be re-exported for the suite's derivations")
}

// TestClaimWithAllocation_NeverSecondGuessesAChoice: an explicitly
// chosen release fails with the named holder — no walking.
func TestClaimWithAllocation_NeverSecondGuessesAChoice(t *testing.T) {
	t.Setenv(EnvNamespace, "")
	t.Setenv(EnvRelease, "")
	t.Setenv(EnvKubecontext, "")
	t.Setenv(EnvCIRunNumber, "")
	t.Setenv(EnvCIRunAttempt, "")

	s := &stubRunner{}
	s.on("kubectl get lease gemaal-claim-chosen", leaseJSON("other@host#7", 5*time.Second, 90), nil)

	suite := Suite{Options: Options{Release: "chosen"}}
	cluster := &Cluster{Runner: s}
	tenant := Tenant{Namespace: "emp-jdoe", Release: "chosen"}

	_, got, err := suite.claimWithAllocation(context.Background(), cluster, tenant)

	var held ErrTenantHeld
	require.ErrorAs(t, err, &held)
	assert.Equal(t, "other@host#7", held.Holder)
	assert.Equal(t, "chosen", got.Release, "no lane walk on a chosen name")
}

// sweepLease is one fixture entry for the sweep tests.
type sweepLease struct {
	name    string
	renew   string // RenewTime literal; empty exercises the acquire fallback
	acquire string
}

// sweepListJSON builds a LeaseList of gemaal-labeled claims.
func sweepListJSON(t *testing.T, entries []sweepLease) string {
	t.Helper()

	var items []leaseObject

	for _, e := range entries {
		var l leaseObject
		l.Metadata.Name = e.name
		l.Metadata.Namespace = "ci-truvity-bar"
		l.Spec.HolderIdentity = "someone@somewhere#1"
		l.Spec.LeaseDurationSeconds = int(LeaseDuration / time.Second)
		l.Spec.RenewTime = e.renew
		l.Spec.AcquireTime = e.acquire
		items = append(items, l)
	}

	data, err := json.Marshal(map[string]any{"items": items})
	require.NoError(t, err)

	return string(data)
}

// TestSweepAbandonedClaims_HourColdOnly: the sweep deletes only leases
// silent for sweepClaimAfter. Freshly-renewed claims are HELD; an
// expired-but-recent lease has freed its lane yet keeps its object (the
// margin exists so a delete never races a takeover's compare-and-swap);
// an unparseable timestamp stands down rather than guessing; a lease
// with only an acquire time falls back to it.
func TestSweepAbandonedClaims_HourColdOnly(t *testing.T) {
	s := &stubRunner{}
	cluster := &Cluster{Runner: s}

	old := time.Now().UTC().Add(-2 * time.Hour).Format(leaseTimeLayout)
	recent := time.Now().UTC().Add(-10 * time.Minute).Format(leaseTimeLayout)
	fresh := time.Now().UTC().Add(-5 * time.Second).Format(leaseTimeLayout)

	list := sweepListJSON(t, []sweepLease{
		{name: "gemaal-claim-dead-run", renew: old},
		{name: "gemaal-claim-live-run", renew: fresh},
		{name: "gemaal-claim-expired-new", renew: recent},
		{name: "gemaal-claim-garbled", renew: "not-a-timestamp"},
		{name: "gemaal-claim-never-renewed", acquire: old},
	})

	s.on("kubectl get leases", list, nil)

	cluster.SweepAbandonedClaims(context.Background(), "ci-truvity-bar")

	var deletes []string

	for _, call := range s.joined() {
		if strings.Contains(call, "kubectl delete lease") {
			deletes = append(deletes, call)
		}
	}

	require.Len(t, deletes, 1, "one batched delete for all abandoned leases")
	assert.Contains(t, deletes[0], "gemaal-claim-dead-run")
	assert.NotContains(t, deletes[0], "gemaal-claim-live-run", "a renewing claim is held")
	assert.NotContains(t, deletes[0], "gemaal-claim-expired-new",
		"lane-free but object-young: the margin protects takeover CAS")
	assert.NotContains(t, deletes[0], "gemaal-claim-garbled", "when in doubt, stand down")
	assert.Contains(t, deletes[0], "gemaal-claim-never-renewed",
		"a lease that never renewed falls back to its acquire time")
	assert.Contains(t, deletes[0], "--namespace ci-truvity-bar")
	assert.Contains(t, deletes[0], "--ignore-not-found")

	var listed string

	for _, call := range s.joined() {
		if strings.Contains(call, "kubectl get leases") {
			listed = call
		}
	}

	assert.Contains(t, listed, "app.kubernetes.io/managed-by=gemaal-harness",
		"selection is by gemaal's own label, never by name")
}

// TestSweepAbandonedClaims_BestEffort: an unlistable lease API or an
// empty namespace deletes nothing and, above all, does not fail.
func TestSweepAbandonedClaims_BestEffort(t *testing.T) {
	s := &stubRunner{}
	cluster := &Cluster{Runner: s}
	s.on("kubectl get leases", "", errors.New("api unavailable"))

	cluster.SweepAbandonedClaims(context.Background(), "ci-truvity-bar")

	for _, call := range s.joined() {
		assert.NotContains(t, call, "delete", "no deletes when the list is unavailable")
	}

	s2 := &stubRunner{}
	cluster2 := &Cluster{Runner: s2}
	s2.on("kubectl get leases", `{"items": []}`, nil)

	cluster2.SweepAbandonedClaims(context.Background(), "ci-truvity-bar")

	for _, call := range s2.joined() {
		assert.NotContains(t, call, "delete", "an empty namespace needs no janitor")
	}
}
