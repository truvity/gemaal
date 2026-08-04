// Package service implements the gemaal ConnectRPC API.
//
// G1 status: stubs. Plan answers from an empty in-memory tenant view;
// every other RPC returns CodeUnimplemented. The real drivers and the
// housekeeping engine arrive with G3 (the port of the janitor prototype).
package service

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	gemaalv1 "github.com/truvity/gemaal/gen/gemaal/v1"
	"github.com/truvity/gemaal/gen/gemaal/v1/gemaalv1connect"
	"github.com/truvity/gemaal/pkg/config"
)

// Service serves the gemaal.v1.GemaalService API.
type Service struct {
	cfg *config.Config
	log *slog.Logger
}

var _ gemaalv1connect.GemaalServiceHandler = (*Service)(nil)

// New wires a Service from its configuration.
func New(cfg *config.Config, log *slog.Logger) *Service {
	return &Service{cfg: cfg, log: log}
}

// Handler returns the mount path and HTTP handler for the ConnectRPC API.
func (s *Service) Handler() (string, http.Handler) {
	return gemaalv1connect.NewGemaalServiceHandler(s)
}

// Plan computes what housekeeping would do right now. G1: the tenant view
// is empty, so the plan always is too — but the RPC is real, which is what
// shadow mode needs first.
func (s *Service) Plan(
	ctx context.Context,
	req *connect.Request[gemaalv1.PlanRequest],
) (*connect.Response[gemaalv1.PlanResponse], error) {
	s.log.InfoContext(ctx, "plan requested",
		slog.String("namespace", req.Msg.GetNamespace()),
		slog.String("release", req.Msg.GetRelease()),
	)

	return connect.NewResponse(&gemaalv1.PlanResponse{
		Actions:    nil,
		ComputedAt: timestamppb.Now(),
	}), nil
}

// ListTenants is unimplemented until G3.
func (s *Service) ListTenants(
	context.Context,
	*connect.Request[gemaalv1.ListTenantsRequest],
) (*connect.Response[gemaalv1.ListTenantsResponse], error) {
	return nil, unimplemented("ListTenants")
}

// Checkout is unimplemented until G3.
func (s *Service) Checkout(
	context.Context,
	*connect.Request[gemaalv1.CheckoutRequest],
) (*connect.Response[gemaalv1.CheckoutResponse], error) {
	return nil, unimplemented("Checkout")
}

// Extend is unimplemented until G3.
func (s *Service) Extend(
	context.Context,
	*connect.Request[gemaalv1.ExtendRequest],
) (*connect.Response[gemaalv1.ExtendResponse], error) {
	return nil, unimplemented("Extend")
}

// Sweep is unimplemented until G3.
func (s *Service) Sweep(
	context.Context,
	*connect.Request[gemaalv1.SweepRequest],
) (*connect.Response[gemaalv1.SweepResponse], error) {
	return nil, unimplemented("Sweep")
}

// Resolve is unimplemented until G3.
func (s *Service) Resolve(
	context.Context,
	*connect.Request[gemaalv1.ResolveRequest],
) (*connect.Response[gemaalv1.ResolveResponse], error) {
	return nil, unimplemented("Resolve")
}

func unimplemented(rpc string) error {
	return connect.NewError(
		connect.CodeUnimplemented,
		fmt.Errorf("%s is not implemented yet — it arrives with the G3 service milestone", rpc),
	)
}
