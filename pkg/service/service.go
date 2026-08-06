// Package service implements the gemaal ConnectRPC API — the six RPCs
// of proto/gemaal/v1 over the housekeeping engine.
//
// Faces: Plan and Sweep are the janitor, ListTenants the console,
// Checkout and Extend the tenant-lifetime face, Resolve the identity
// authority. Mutations authenticate through pkg/authn (TokenReview for
// workloads, the gateway-forwarded JWT for humans) and authorize in
// authz.go; reads are open — the deployment's gateway and network
// policy bound who can reach them at all.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	gemaalv1 "github.com/truvity/gemaal/gen/gemaal/v1"
	"github.com/truvity/gemaal/gen/gemaal/v1/gemaalv1connect"
	"github.com/truvity/gemaal/pkg/authn"
	"github.com/truvity/gemaal/pkg/config"
	"github.com/truvity/gemaal/pkg/engine"
	"github.com/truvity/gemaal/pkg/identity"
)

// Housekeeper is the engine surface the RPCs drive. *engine.Engine
// satisfies it; tests inject fakes.
type Housekeeper interface {
	View(ctx context.Context) (*engine.View, error)
	Plan(ctx context.Context, narrow engine.Narrow) (*engine.Plan, error)
	Apply(ctx context.Context, plan *engine.Plan, confirm bool, source string) (*engine.SweepRecord, error)
	StampKeepUntil(ctx context.Context, tenant engine.Tenant, until time.Time) error
}

// Authenticator resolves an Authorization header to an identity.
// *authn.Authenticator satisfies it; tests inject fakes.
type Authenticator interface {
	Authenticate(ctx context.Context, authorization string) (authn.Identity, error)
}

// Deps wires a Service.
type Deps struct {
	Config  *config.Config
	Logger  *slog.Logger
	Keeper  Housekeeper
	Auth    Authenticator
	History *engine.History
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Service serves the gemaal.v1.GemaalService API.
type Service struct {
	deps Deps
}

var _ gemaalv1connect.GemaalServiceHandler = (*Service)(nil)

// rfc1123Label is the shape both halves of a tenant identity must have.
var rfc1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// New wires a Service.
func New(deps Deps) *Service {
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}

	if deps.Now == nil {
		deps.Now = time.Now
	}

	if deps.History == nil {
		deps.History = engine.NewHistory(0)
	}

	return &Service{deps: deps}
}

// Handler returns the mount path and HTTP handler for the ConnectRPC API.
func (s *Service) Handler() (string, http.Handler) {
	return gemaalv1connect.NewGemaalServiceHandler(s)
}

// History exposes the sweep records for the panel.
func (s *Service) History() *engine.History { return s.deps.History }

// Plan computes what housekeeping would do right now. No side effects,
// no authentication: the plan is the console's honesty, not a mutation.
func (s *Service) Plan(
	ctx context.Context,
	req *connect.Request[gemaalv1.PlanRequest],
) (*connect.Response[gemaalv1.PlanResponse], error) {
	plan, err := s.deps.Keeper.Plan(ctx, engine.Narrow{
		Namespace: req.Msg.GetNamespace(),
		Release:   req.Msg.GetRelease(),
	})
	if err != nil {
		s.deps.Logger.ErrorContext(ctx, "plan failed", slog.Any("error", err))

		return nil, connect.NewError(connect.CodeInternal, errors.New("could not compute the plan"))
	}

	resp := &gemaalv1.PlanResponse{ComputedAt: timestamppb.New(plan.GeneratedAt)}

	for i := range plan.Delete {
		resp.Actions = append(resp.Actions, plannedAction(&plan.Delete[i]))
	}

	return connect.NewResponse(resp), nil
}

// ListTenants enumerates the watched installations with ledger state and
// the action the loop would take next.
func (s *Service) ListTenants(
	ctx context.Context,
	req *connect.Request[gemaalv1.ListTenantsRequest],
) (*connect.Response[gemaalv1.ListTenantsResponse], error) {
	view, err := s.deps.Keeper.View(ctx)
	if err != nil {
		s.deps.Logger.ErrorContext(ctx, "list tenants failed", slog.Any("error", err))

		return nil, connect.NewError(connect.CodeInternal, errors.New("could not build the tenant view"))
	}

	resp := &gemaalv1.ListTenantsResponse{}

	for i := range view.Tenants {
		state := &view.Tenants[i]

		if tier := req.Msg.GetTier(); tier != "" && state.Tier != tier {
			continue
		}

		if ns := req.Msg.GetNamespace(); ns != "" && state.Tenant.Namespace != ns {
			continue
		}

		resp.Tenants = append(resp.Tenants, tenantProto(state, view.At))
	}

	return connect.NewResponse(resp), nil
}

// Checkout claims a tenant slot for the caller: keep-until = now +
// duration (bounded by hold.max).
func (s *Service) Checkout(
	ctx context.Context,
	req *connect.Request[gemaalv1.CheckoutRequest],
) (*connect.Response[gemaalv1.CheckoutResponse], error) {
	state, until, err := s.hold(ctx, req.Header(), req.Msg.GetNamespace(), req.Msg.GetRelease(), req.Msg.GetDuration(), false)
	if err != nil {
		return nil, err
	}

	state.Ledger.KeepUntil = until

	return connect.NewResponse(&gemaalv1.CheckoutResponse{Tenant: tenantProto(&state, s.deps.Now())}), nil
}

// Extend pushes a tenant's keep-until forward — never backwards.
func (s *Service) Extend(
	ctx context.Context,
	req *connect.Request[gemaalv1.ExtendRequest],
) (*connect.Response[gemaalv1.ExtendResponse], error) {
	state, until, err := s.hold(ctx, req.Header(), req.Msg.GetNamespace(), req.Msg.GetRelease(), req.Msg.GetDuration(), true)
	if err != nil {
		return nil, err
	}

	state.Ledger.KeepUntil = until

	return connect.NewResponse(&gemaalv1.ExtendResponse{Tenant: tenantProto(&state, s.deps.Now())}), nil
}

// hold implements the shared Checkout/Extend path: authenticate,
// validate, authorize, stamp.
func (s *Service) hold(
	ctx context.Context,
	header http.Header,
	namespace, release string,
	duration *durationpb.Duration,
	forward bool,
) (engine.TenantState, time.Time, error) {
	var none engine.TenantState

	caller, err := s.authenticate(ctx, header)
	if err != nil {
		return none, time.Time{}, err
	}

	if err := validName(namespace, "namespace"); err != nil {
		return none, time.Time{}, connect.NewError(connect.CodeInvalidArgument, err)
	}

	if err := validName(release, "release"); err != nil {
		return none, time.Time{}, connect.NewError(connect.CodeInvalidArgument, err)
	}

	if err := s.authorizeHold(caller, namespace); err != nil {
		return none, time.Time{}, err
	}

	view, err := s.deps.Keeper.View(ctx)
	if err != nil {
		s.deps.Logger.ErrorContext(ctx, "hold failed building the view", slog.Any("error", err))

		return none, time.Time{}, connect.NewError(connect.CodeInternal, errors.New("could not build the tenant view"))
	}

	state, found := view.Find(namespace, release)
	if !found {
		return none, time.Time{}, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("tenant %s/%s is not watched (no release, or the namespace carries no configured tier label)", namespace, release))
	}

	d := s.deps.Config.Hold.Default.Std()
	if duration != nil {
		d = duration.AsDuration()
	}

	if d <= 0 {
		return none, time.Time{}, connect.NewError(connect.CodeInvalidArgument, errors.New("duration must be positive"))
	}

	if maxHold := s.deps.Config.Hold.Max.Std(); d > maxHold {
		d = maxHold
	}

	until := s.deps.Now().Add(d)
	if forward && state.Ledger.KeepUntil.After(until) {
		// Extend never moves a hold backwards.
		until = state.Ledger.KeepUntil
	}

	if err := s.deps.Keeper.StampKeepUntil(ctx, state.Tenant, until); err != nil {
		s.deps.Logger.ErrorContext(ctx, "keep-until stamp failed",
			slog.String("tenant", state.Tenant.String()), slog.Any("error", err))

		return none, time.Time{}, connect.NewError(connect.CodeInternal, errors.New("could not stamp keep-until"))
	}

	s.deps.Logger.InfoContext(ctx, "tenant held",
		slog.String("tenant", state.Tenant.String()),
		slog.String("caller", caller.Subject),
		slog.String("method", caller.Method),
		slog.Time("keep_until", until),
	)

	return state, until, nil
}

// Sweep executes the current plan, subject to the server's dry-run
// setting — a dry-run server reports and deletes nothing, whatever the
// request says.
func (s *Service) Sweep(
	ctx context.Context,
	req *connect.Request[gemaalv1.SweepRequest],
) (*connect.Response[gemaalv1.SweepResponse], error) {
	caller, err := s.authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}

	if err := s.authorizeSweep(caller); err != nil {
		return nil, err
	}

	plan, err := s.deps.Keeper.Plan(ctx, engine.Narrow{Namespace: req.Msg.GetNamespace()})
	if err != nil {
		s.deps.Logger.ErrorContext(ctx, "sweep failed planning", slog.Any("error", err))

		return nil, connect.NewError(connect.CodeInternal, errors.New("could not compute the plan"))
	}

	// The server's setting wins; a request can only make things drier.
	dryRun := *s.deps.Config.DryRun || req.Msg.GetDryRun()

	record, err := s.deps.Keeper.Apply(ctx, plan, !dryRun, "rpc")
	if record != nil {
		s.deps.History.Add(*record)
	}

	if err != nil && (record == nil || len(record.Results) == 0) {
		// Nothing even started — refused wholesale (authorization rail).
		s.deps.Logger.ErrorContext(ctx, "sweep refused", slog.Any("error", err))

		return nil, connect.NewError(connect.CodeInternal, errors.New("sweep refused before execution"))
	}

	resp := &gemaalv1.SweepResponse{DryRun: dryRun}
	for i := range record.Results {
		resp.Results = append(resp.Results, &gemaalv1.SweepResult{
			Action:   plannedAction(&record.Results[i].Action),
			Executed: record.Results[i].Executed,
			Error:    record.Results[i].Err,
		})
	}

	s.deps.Logger.InfoContext(ctx, "sweep executed",
		slog.String("caller", caller.Subject),
		slog.Bool("dry_run", dryRun),
		slog.Int("actions", len(record.Results)),
	)

	return connect.NewResponse(resp), nil
}

// Resolve maps an email to its slug and standing namespace — the
// resolution authority, from the deployer-rendered map.
func (s *Service) Resolve(
	ctx context.Context,
	req *connect.Request[gemaalv1.ResolveRequest],
) (*connect.Response[gemaalv1.ResolveResponse], error) {
	email := req.Msg.GetEmail()
	if email == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("email is required"))
	}

	slug, err := identity.MapResolver(s.deps.Config.Identity.Emails).Resolve(ctx, email)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}

	return connect.NewResponse(&gemaalv1.ResolveResponse{
		Slug:      slug,
		Namespace: s.deps.Config.PersonalNamespace(slug),
	}), nil
}

// authenticate maps authn errors onto connect codes.
func (s *Service) authenticate(ctx context.Context, header http.Header) (authn.Identity, error) {
	if s.deps.Auth == nil {
		return authn.Identity{}, connect.NewError(connect.CodeUnauthenticated, errors.New("no authenticator configured"))
	}

	caller, err := s.deps.Auth.Authenticate(ctx, header.Get("Authorization"))
	if err != nil {
		if errors.Is(err, authn.ErrNoCredentials) {
			return authn.Identity{}, connect.NewError(connect.CodeUnauthenticated, err)
		}

		s.deps.Logger.WarnContext(ctx, "authentication failed", slog.Any("error", err))

		return authn.Identity{}, connect.NewError(connect.CodeUnauthenticated, errors.New("could not authenticate the caller"))
	}

	return caller, nil
}

// plannedAction converts an engine action to its proto form.
func plannedAction(action *engine.Action) *gemaalv1.PlannedAction {
	kind := gemaalv1.ActionKind_ACTION_KIND_SWEEP
	if action.Item.Kind == engine.KindHelmRelease {
		kind = gemaalv1.ActionKind_ACTION_KIND_TEARDOWN
	}

	return &gemaalv1.PlannedAction{
		Kind:      kind,
		Namespace: action.Item.Tenant.Namespace,
		Release:   action.Item.Tenant.Release,
		Target:    action.Item.ID,
		Rule:      string(action.Rule),
		Reason:    action.Reason,
	}
}

// tenantProto converts a tenant state to its proto form.
func tenantProto(state *engine.TenantState, at time.Time) *gemaalv1.Tenant {
	pending := gemaalv1.ActionKind_ACTION_KIND_NONE
	if state.Expired(at) {
		pending = gemaalv1.ActionKind_ACTION_KIND_TEARDOWN
	}

	tenant := &gemaalv1.Tenant{
		Namespace:     state.Tenant.Namespace,
		Release:       state.Tenant.Release,
		Backend:       "helm",
		Tier:          state.Tier,
		LastActivity:  timestamppb.New(state.LastActivity),
		Age:           durationpb.New(state.Age(at)),
		Ttl:           durationpb.New(state.TTL),
		ExecutionId:   state.Ledger.ExecutionID,
		PendingAction: pending,
	}

	if !state.Ledger.KeepUntil.IsZero() {
		tenant.KeepUntil = timestamppb.New(state.Ledger.KeepUntil)
	}

	return tenant
}

func validName(name, kind string) error {
	const maxLen = 63

	if name == "" {
		return fmt.Errorf("empty %s", kind)
	}

	if len(name) > maxLen || !rfc1123Label.MatchString(name) {
		return fmt.Errorf("%s %q is not a valid RFC1123 label", kind, name)
	}

	return nil
}
