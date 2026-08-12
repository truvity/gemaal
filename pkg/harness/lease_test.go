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
