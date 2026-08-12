package service_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	gemaalv1 "github.com/truvity/gemaal/gen/gemaal/v1"
	"github.com/truvity/gemaal/gen/gemaal/v1/gemaalv1connect"
	"github.com/truvity/gemaal/pkg/authn"
	"github.com/truvity/gemaal/pkg/config"
	"github.com/truvity/gemaal/pkg/engine"
	"github.com/truvity/gemaal/pkg/service"
)

var now = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

// fakeKeeper scripts the Housekeeper seam and records what the RPCs
// asked of it.
type fakeKeeper struct {
	view    *engine.View
	viewErr error

	plan    *engine.Plan
	planErr error

	record   *engine.SweepRecord
	applyErr error

	stamped  []string // "<ns>/<rel> until <ts>"
	stampErr error

	applies []bool // confirm flags, in order

	decommissioned  []string // "<ns>/<rel> confirm=<bool>"
	decommissionErr error
	decommissionRec *engine.SweepRecord
}

func (f *fakeKeeper) View(context.Context) (*engine.View, error) { return f.view, f.viewErr }

func (f *fakeKeeper) Plan(context.Context, engine.Narrow) (*engine.Plan, error) {
	return f.plan, f.planErr
}

func (f *fakeKeeper) Apply(_ context.Context, _ *engine.Plan, confirm bool, _ string) (*engine.SweepRecord, error) {
	f.applies = append(f.applies, confirm)

	return f.record, f.applyErr
}

func (f *fakeKeeper) StampKeepUntil(_ context.Context, tenant engine.Tenant, until time.Time) error {
	if f.stampErr != nil {
		return f.stampErr
	}

	f.stamped = append(f.stamped, tenant.String()+" until "+engine.FormatKeepUntil(until))

	return nil
}

func (f *fakeKeeper) Decommission(_ context.Context, namespace, release string, confirm bool) (*engine.SweepRecord, error) {
	f.decommissioned = append(f.decommissioned, fmt.Sprintf("%s/%s confirm=%v", namespace, release, confirm))

	return f.decommissionRec, f.decommissionErr
}

// fakeAuth scripts the authenticator: identities keyed by bearer token.
type fakeAuth struct {
	identities map[string]authn.Identity
}

func (f *fakeAuth) Authenticate(_ context.Context, authorization string) (authn.Identity, error) {
	const scheme = "Bearer "
	if len(authorization) <= len(scheme) {
		return authn.Identity{}, authn.ErrNoCredentials
	}

	identity, ok := f.identities[authorization[len(scheme):]]
	if !ok {
		return authn.Identity{}, errors.New("unknown token")
	}

	return identity, nil
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()

	cfg, err := config.Parse([]byte(`
tiers:
  employee:
    ttl: 14h
  ci:
    ttl: 24h
identity:
  emails:
    j.doe@example.com: jdoe
authz:
  adminGroups: ["cluster-devel:cluster:admin"]
hold:
  default: 8h
  max: 72h
`))
	require.NoError(t, err)

	return cfg
}

// testView is one fresh employee tenant and one expired CI tenant.
func testView() *engine.View {
	return &engine.View{
		At: now,
		Namespaces: []engine.Namespace{
			{Name: "ci-truvity-bar", Tier: "ci"},
			{Name: "emp-jdoe", Tier: "employee"},
		},
		Tenants: []engine.TenantState{
			{
				Tenant:       engine.Tenant{Namespace: "ci-truvity-bar", Release: "r1-a1"},
				Tier:         "ci",
				Releases:     []engine.Release{{Namespace: "ci-truvity-bar", Name: "r1-a1", Updated: now.Add(-30 * time.Hour)}},
				LastActivity: now.Add(-30 * time.Hour),
				TTL:          24 * time.Hour,
				TTLSource:    "tier-default",
			},
			{
				Tenant:       engine.Tenant{Namespace: "emp-jdoe", Release: "myapp"},
				Tier:         "employee",
				Releases:     []engine.Release{{Namespace: "emp-jdoe", Name: "myapp", Updated: now.Add(-2 * time.Hour)}},
				Ledger:       engine.Ledger{ExecutionID: "local-1"},
				LastActivity: now.Add(-2 * time.Hour),
				TTL:          14 * time.Hour,
				TTLSource:    "tier-default",
			},
		},
	}
}

// tokens of the cast: an owner, an admin, a foreigner, a CI workload.
var identities = map[string]authn.Identity{
	"owner-group": {Subject: "123", Email: "someone@example.com", Groups: []string{"emp:jdoe"}, Method: authn.MethodGatewayJWT},
	"owner-email": {Subject: "j.doe@example.com", Email: "J.Doe@example.com", Method: authn.MethodGatewayJWT},
	"admin":       {Subject: "root@example.com", Email: "root@example.com", Groups: []string{"cluster-devel:cluster:admin"}, Method: authn.MethodGatewayJWT},
	"foreigner":   {Subject: "x@example.com", Email: "x@example.com", Groups: []string{"emp:other"}, Method: authn.MethodGatewayJWT},
	"workload":    {Subject: "system:serviceaccount:ci-truvity-bar:tester", Groups: []string{"system:serviceaccounts"}, Method: authn.MethodTokenReview},
}

func newTestService(t *testing.T, cfg *config.Config, keeper *fakeKeeper) (gemaalv1connect.GemaalServiceClient, *engine.History) {
	t.Helper()

	if cfg == nil {
		cfg = testConfig(t)
	}

	history := engine.NewHistory(10)

	svc := service.New(service.Deps{
		Config:  cfg,
		Logger:  slog.New(slog.DiscardHandler),
		Keeper:  keeper,
		Auth:    &fakeAuth{identities: identities},
		History: history,
		Now:     func() time.Time { return now },
	})

	mux := http.NewServeMux()
	path, handler := svc.Handler()
	mux.Handle(path, handler)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return gemaalv1connect.NewGemaalServiceClient(server.Client(), server.URL), history
}

func bearer[T any](req *connect.Request[T], token string) *connect.Request[T] {
	req.Header().Set("Authorization", "Bearer "+token)

	return req
}

// --- Plan -----------------------------------------------------------------

func TestPlanMapsActions(t *testing.T) {
	keeper := &fakeKeeper{plan: &engine.Plan{
		GeneratedAt: now,
		Delete: []engine.Action{
			{
				Item: engine.Item{Kind: engine.KindHelmRelease,
					Tenant: engine.Tenant{Namespace: "ci-truvity-bar", Release: "r1-a1"}, ID: "ci-truvity-bar/r1-a1"},
				Rule:   engine.RuleTTLExpired,
				Reason: "idle 30h exceeds ttl 24h",
			},
			{
				Item: engine.Item{Kind: engine.KindSSMPrefix,
					Tenant: engine.Tenant{Namespace: "emp-gone", Release: "myapp"}, ID: "/test/emp-gone/myapp/"},
				Rule: engine.RuleOrphanGrace,
			},
		},
	}}

	client, _ := newTestService(t, nil, keeper)

	resp, err := client.Plan(context.Background(), connect.NewRequest(&gemaalv1.PlanRequest{}))
	require.NoError(t, err)

	actions := resp.Msg.GetActions()
	require.Len(t, actions, 2)
	assert.Equal(t, gemaalv1.ActionKind_ACTION_KIND_TEARDOWN, actions[0].GetKind())
	assert.Equal(t, "ttl-expired", actions[0].GetRule())
	assert.Equal(t, "ci-truvity-bar/r1-a1", actions[0].GetTarget())
	assert.Equal(t, gemaalv1.ActionKind_ACTION_KIND_SWEEP, actions[1].GetKind())
}

// --- ListTenants ----------------------------------------------------------

func TestListTenants(t *testing.T) {
	client, _ := newTestService(t, nil, &fakeKeeper{view: testView()})

	resp, err := client.ListTenants(context.Background(), connect.NewRequest(&gemaalv1.ListTenantsRequest{}))
	require.NoError(t, err)

	tenants := resp.Msg.GetTenants()
	require.Len(t, tenants, 2)

	assert.Equal(t, "ci", tenants[0].GetTier())
	assert.Equal(t, gemaalv1.ActionKind_ACTION_KIND_TEARDOWN, tenants[0].GetPendingAction(), "30h idle at 24h ttl")
	assert.Equal(t, 30*time.Hour, tenants[0].GetAge().AsDuration())

	assert.Equal(t, "emp-jdoe", tenants[1].GetNamespace())
	assert.Equal(t, gemaalv1.ActionKind_ACTION_KIND_NONE, tenants[1].GetPendingAction())
	assert.Equal(t, "local-1", tenants[1].GetExecutionId())
	assert.Nil(t, tenants[1].GetKeepUntil())
}

func TestListTenantsFilters(t *testing.T) {
	client, _ := newTestService(t, nil, &fakeKeeper{view: testView()})

	resp, err := client.ListTenants(context.Background(),
		connect.NewRequest(&gemaalv1.ListTenantsRequest{Tier: "employee"}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetTenants(), 1)
	assert.Equal(t, "emp-jdoe", resp.Msg.GetTenants()[0].GetNamespace())

	resp, err = client.ListTenants(context.Background(),
		connect.NewRequest(&gemaalv1.ListTenantsRequest{Namespace: "ci-truvity-bar"}))
	require.NoError(t, err)
	require.Len(t, resp.Msg.GetTenants(), 1)
	assert.Equal(t, "r1-a1", resp.Msg.GetTenants()[0].GetRelease())
}

// --- Checkout / Extend: the authorization matrix --------------------------

func TestHoldAuthorizationMatrix(t *testing.T) {
	cases := []struct {
		name      string
		token     string
		namespace string
		wantCode  connect.Code
		allowed   bool
	}{
		{name: "owner via emp:{slug} group", token: "owner-group", namespace: "emp-jdoe", allowed: true},
		{name: "owner via resolved email (case-insensitive)", token: "owner-email", namespace: "emp-jdoe", allowed: true},
		{name: "admin may hold any tenant", token: "admin", namespace: "ci-truvity-bar", allowed: true},
		{name: "foreign slug refused", token: "foreigner", namespace: "emp-jdoe", wantCode: connect.CodePermissionDenied},
		{name: "workload without ownership refused", token: "workload", namespace: "ci-truvity-bar", wantCode: connect.CodePermissionDenied},
		{name: "no credentials refused", token: "", wantCode: connect.CodeUnauthenticated, namespace: "emp-jdoe"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keeper := &fakeKeeper{view: testView()}
			client, _ := newTestService(t, nil, keeper)

			release := "myapp"
			if tc.namespace == "ci-truvity-bar" {
				release = "r1-a1"
			}

			req := connect.NewRequest(&gemaalv1.CheckoutRequest{Namespace: tc.namespace, Release: release})
			if tc.token != "" {
				bearer(req, tc.token)
			}

			_, err := client.Checkout(context.Background(), req)

			if tc.allowed {
				require.NoError(t, err)
				require.Len(t, keeper.stamped, 1)
			} else {
				require.Equal(t, tc.wantCode, connect.CodeOf(err))
				assert.Empty(t, keeper.stamped, "a refused hold stamps nothing")
			}
		})
	}
}

func TestSweepAuthorizationMatrix(t *testing.T) {
	for _, tc := range []struct {
		name     string
		token    string
		wantCode connect.Code
		allowed  bool
	}{
		{name: "admin group sweeps", token: "admin", allowed: true},
		{name: "owner is not admin", token: "owner-group", wantCode: connect.CodePermissionDenied},
		{name: "workload is not admin", token: "workload", wantCode: connect.CodePermissionDenied},
		{name: "anonymous refused", token: "", wantCode: connect.CodeUnauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keeper := &fakeKeeper{plan: &engine.Plan{}, record: &engine.SweepRecord{At: now, DryRun: true}}
			client, _ := newTestService(t, nil, keeper)

			req := connect.NewRequest(&gemaalv1.SweepRequest{})
			if tc.token != "" {
				bearer(req, tc.token)
			}

			_, err := client.Sweep(context.Background(), req)

			if tc.allowed {
				require.NoError(t, err)
				assert.NotEmpty(t, keeper.applies)
			} else {
				require.Equal(t, tc.wantCode, connect.CodeOf(err))
				assert.Empty(t, keeper.applies, "a refused sweep applies nothing")
			}
		})
	}
}

// --- Checkout / Extend semantics ------------------------------------------

func TestCheckoutStampsNowPlusDuration(t *testing.T) {
	keeper := &fakeKeeper{view: testView()}
	client, _ := newTestService(t, nil, keeper)

	req := bearer(connect.NewRequest(&gemaalv1.CheckoutRequest{
		Namespace: "emp-jdoe", Release: "myapp", Duration: durationpb.New(4 * time.Hour),
	}), "owner-group")

	resp, err := client.Checkout(context.Background(), req)
	require.NoError(t, err)

	until := now.Add(4 * time.Hour)
	assert.Equal(t, []string{"emp-jdoe/myapp until " + engine.FormatKeepUntil(until)}, keeper.stamped)
	assert.Equal(t, until, resp.Msg.GetTenant().GetKeepUntil().AsTime())
	assert.Equal(t, gemaalv1.ActionKind_ACTION_KIND_NONE, resp.Msg.GetTenant().GetPendingAction())
}

func TestCheckoutDefaultsAndClampsDuration(t *testing.T) {
	keeper := &fakeKeeper{view: testView()}
	client, _ := newTestService(t, nil, keeper)

	// No duration: the configured default (8h).
	_, err := client.Checkout(context.Background(), bearer(connect.NewRequest(
		&gemaalv1.CheckoutRequest{Namespace: "emp-jdoe", Release: "myapp"}), "owner-group"))
	require.NoError(t, err)
	assert.Contains(t, keeper.stamped[0], engine.FormatKeepUntil(now.Add(8*time.Hour)))

	// A greedy duration is clamped to hold.max (72h), not refused.
	_, err = client.Checkout(context.Background(), bearer(connect.NewRequest(
		&gemaalv1.CheckoutRequest{Namespace: "emp-jdoe", Release: "myapp", Duration: durationpb.New(1000 * time.Hour)}), "owner-group"))
	require.NoError(t, err)
	assert.Contains(t, keeper.stamped[1], engine.FormatKeepUntil(now.Add(72*time.Hour)))

	// A non-positive duration is nonsense, not a default.
	_, err = client.Checkout(context.Background(), bearer(connect.NewRequest(
		&gemaalv1.CheckoutRequest{Namespace: "emp-jdoe", Release: "myapp", Duration: durationpb.New(-time.Hour)}), "owner-group"))
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
}

func TestExtendNeverMovesTheHoldBackwards(t *testing.T) {
	view := testView()
	view.Tenants[1].Ledger.KeepUntil = now.Add(48 * time.Hour)

	keeper := &fakeKeeper{view: view}
	client, _ := newTestService(t, nil, keeper)

	// now+2h is BEFORE the standing 48h hold: the hold stays.
	resp, err := client.Extend(context.Background(), bearer(connect.NewRequest(
		&gemaalv1.ExtendRequest{Namespace: "emp-jdoe", Release: "myapp", Duration: durationpb.New(2 * time.Hour)}), "owner-group"))
	require.NoError(t, err)
	assert.Equal(t, now.Add(48*time.Hour), resp.Msg.GetTenant().GetKeepUntil().AsTime())

	// now+60h is past it: forward it moves.
	resp, err = client.Extend(context.Background(), bearer(connect.NewRequest(
		&gemaalv1.ExtendRequest{Namespace: "emp-jdoe", Release: "myapp", Duration: durationpb.New(60 * time.Hour)}), "owner-group"))
	require.NoError(t, err)
	assert.Equal(t, now.Add(60*time.Hour), resp.Msg.GetTenant().GetKeepUntil().AsTime())
}

func TestHoldUnknownTenant(t *testing.T) {
	client, _ := newTestService(t, nil, &fakeKeeper{view: testView()})

	_, err := client.Checkout(context.Background(), bearer(connect.NewRequest(
		&gemaalv1.CheckoutRequest{Namespace: "emp-jdoe", Release: "ghost"}), "owner-group"))
	assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
}

func TestHoldInvalidNames(t *testing.T) {
	client, _ := newTestService(t, nil, &fakeKeeper{view: testView()})

	_, err := client.Checkout(context.Background(), bearer(connect.NewRequest(
		&gemaalv1.CheckoutRequest{Namespace: "Emp Jdoe", Release: "myapp"}), "admin"))
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
}

// --- Sweep semantics ------------------------------------------------------

func TestSweepShadowServerNeverConfirms(t *testing.T) {
	// The server is dry-run (the default config): even an explicit
	// dry_run=false request must not execute.
	keeper := &fakeKeeper{
		plan: &engine.Plan{},
		record: &engine.SweepRecord{At: now, DryRun: true, Results: []engine.ActionResult{
			{Action: engine.Action{Rule: engine.RuleTTLExpired}, Executed: false},
		}},
	}
	client, history := newTestService(t, nil, keeper)

	resp, err := client.Sweep(context.Background(),
		bearer(connect.NewRequest(&gemaalv1.SweepRequest{DryRun: false}), "admin"))
	require.NoError(t, err)

	assert.True(t, resp.Msg.GetDryRun())
	require.Equal(t, []bool{false}, keeper.applies, "the keeper was told confirm=false — shadow mode performs nothing")
	require.Len(t, resp.Msg.GetResults(), 1)
	assert.False(t, resp.Msg.GetResults()[0].GetExecuted())

	assert.Len(t, history.List(), 1, "the record lands in the sweep history")
}

func TestSweepExecutingServer(t *testing.T) {
	cfg := testConfig(t)
	confirmed := false
	cfg.DryRun = &confirmed

	keeper := &fakeKeeper{
		plan:   &engine.Plan{},
		record: &engine.SweepRecord{At: now, Results: []engine.ActionResult{{Executed: true, Deleted: 1}}},
	}
	client, _ := newTestService(t, cfg, keeper)

	resp, err := client.Sweep(context.Background(), bearer(connect.NewRequest(&gemaalv1.SweepRequest{}), "admin"))
	require.NoError(t, err)
	assert.False(t, resp.Msg.GetDryRun())
	assert.Equal(t, []bool{true}, keeper.applies)

	// The request-side dry_run can still make an executing server drier.
	resp, err = client.Sweep(context.Background(),
		bearer(connect.NewRequest(&gemaalv1.SweepRequest{DryRun: true}), "admin"))
	require.NoError(t, err)
	assert.True(t, resp.Msg.GetDryRun())
	assert.Equal(t, []bool{true, false}, keeper.applies)
}

func TestSweepWhollyRefusedIsAnError(t *testing.T) {
	keeper := &fakeKeeper{
		plan:     &engine.Plan{},
		record:   &engine.SweepRecord{At: now},
		applyErr: errors.New("out of reach"),
	}
	client, _ := newTestService(t, nil, keeper)

	_, err := client.Sweep(context.Background(), bearer(connect.NewRequest(&gemaalv1.SweepRequest{}), "admin"))
	assert.Equal(t, connect.CodeInternal, connect.CodeOf(err))
}

// --- Resolve --------------------------------------------------------------

func TestResolve(t *testing.T) {
	client, _ := newTestService(t, nil, &fakeKeeper{})

	resp, err := client.Resolve(context.Background(),
		connect.NewRequest(&gemaalv1.ResolveRequest{Email: "j.doe@example.com"}))
	require.NoError(t, err)
	assert.Equal(t, "jdoe", resp.Msg.GetSlug())
	assert.Equal(t, "emp-jdoe", resp.Msg.GetNamespace())

	// Case-insensitive: mail addressing is, and a session name's casing
	// is not evidence of anything.
	resp, err = client.Resolve(context.Background(),
		connect.NewRequest(&gemaalv1.ResolveRequest{Email: "J.DOE@example.com"}))
	require.NoError(t, err)
	assert.Equal(t, "jdoe", resp.Msg.GetSlug())

	_, err = client.Resolve(context.Background(),
		connect.NewRequest(&gemaalv1.ResolveRequest{Email: "stranger@example.com"}))
	assert.Equal(t, connect.CodeNotFound, connect.CodeOf(err))

	_, err = client.Resolve(context.Background(), connect.NewRequest(&gemaalv1.ResolveRequest{}))
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
}

// --- failure mapping ------------------------------------------------------

func TestViewFailureIsInternal(t *testing.T) {
	client, _ := newTestService(t, nil, &fakeKeeper{viewErr: errors.New("cluster down")})

	_, err := client.ListTenants(context.Background(), connect.NewRequest(&gemaalv1.ListTenantsRequest{}))
	assert.Equal(t, connect.CodeInternal, connect.CodeOf(err))

	_, err = client.Checkout(context.Background(), bearer(connect.NewRequest(
		&gemaalv1.CheckoutRequest{Namespace: "emp-jdoe", Release: "myapp"}), "admin"))
	assert.Equal(t, connect.CodeInternal, connect.CodeOf(err))
}

func TestPlanFailureIsInternal(t *testing.T) {
	client, _ := newTestService(t, nil, &fakeKeeper{planErr: errors.New("cluster down")})

	_, err := client.Plan(context.Background(), connect.NewRequest(&gemaalv1.PlanRequest{}))
	assert.Equal(t, connect.CodeInternal, connect.CodeOf(err))
}

// --- Decommission ---------------------------------------------------------

// TestDecommissionAuthorizationMatrix: same rule as Checkout — the
// namespace owner (or an admin) ends their own tenant; nobody else does.
func TestDecommissionAuthorizationMatrix(t *testing.T) {
	for _, tc := range []struct {
		name     string
		token    string
		wantCode connect.Code
		allowed  bool
	}{
		{name: "owner decommissions their namespace", token: "owner-group", allowed: true},
		{name: "admin decommissions anywhere", token: "admin", allowed: true},
		{name: "foreigner refused", token: "foreigner", wantCode: connect.CodePermissionDenied},
		{name: "anonymous refused", token: "", wantCode: connect.CodeUnauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keeper := &fakeKeeper{decommissionRec: &engine.SweepRecord{At: now, Source: "decommission"}}
			client, history := newTestService(t, nil, keeper)

			req := connect.NewRequest(&gemaalv1.DecommissionRequest{Namespace: "emp-jdoe", Release: "myapp"})
			if tc.token != "" {
				bearer(req, tc.token)
			}

			_, err := client.Decommission(context.Background(), req)

			if tc.allowed {
				require.NoError(t, err)
				assert.NotEmpty(t, keeper.decommissioned)
				assert.Len(t, history.List(), 1, "an explicit decommission is a history event")
			} else {
				require.Equal(t, tc.wantCode, connect.CodeOf(err))
				assert.Empty(t, keeper.decommissioned, "a refused decommission touches nothing")
			}
		})
	}
}

// TestDecommissionShadowServerNeverConfirms: the server's dry-run wins —
// the engine is asked with confirm=false and the response says so.
func TestDecommissionShadowServerNeverConfirms(t *testing.T) {
	keeper := &fakeKeeper{decommissionRec: &engine.SweepRecord{At: now, DryRun: true}}
	client, _ := newTestService(t, nil, keeper)

	req := connect.NewRequest(&gemaalv1.DecommissionRequest{Namespace: "emp-jdoe", Release: "myapp"})
	bearer(req, "owner-group")

	resp, err := client.Decommission(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, resp.Msg.GetDryRun())
	assert.Equal(t, []string{"emp-jdoe/myapp confirm=false"}, keeper.decommissioned)
}

// TestDecommissionNoSuchTenantIsNotFound: "already gone" must be
// distinguishable from "failed to remove".
func TestDecommissionNoSuchTenantIsNotFound(t *testing.T) {
	keeper := &fakeKeeper{decommissionErr: fmt.Errorf("wrap: %w", engine.ErrNoSuchTenant)}
	client, _ := newTestService(t, nil, keeper)

	req := connect.NewRequest(&gemaalv1.DecommissionRequest{Namespace: "emp-jdoe", Release: "ghost"})
	bearer(req, "owner-group")

	_, err := client.Decommission(context.Background(), req)
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err))
}

// TestDecommissionClaimedTenantIsFailedPrecondition: a live suite's
// claim refuses the call with the holder named — actionable, and
// distinct from not-found and from execution failure.
func TestDecommissionClaimedTenantIsFailedPrecondition(t *testing.T) {
	keeper := &fakeKeeper{decommissionErr: engine.ErrTenantClaimed{
		Tenant: engine.Tenant{Namespace: "emp-jdoe", Release: "myapp"},
		Holder: "other@host#7",
	}}
	client, _ := newTestService(t, nil, keeper)

	req := connect.NewRequest(&gemaalv1.DecommissionRequest{Namespace: "emp-jdoe", Release: "myapp"})
	bearer(req, "owner-group")

	_, err := client.Decommission(context.Background(), req)
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	assert.Contains(t, err.Error(), "other@host#7")
}
