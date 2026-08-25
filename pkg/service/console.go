package service

// The console-facing read RPCs: who am I, which build, what has the sweeper
// done. Added for the web console (the SPA that replaced the SSR panel);
// gemaalctl has no use for them.

import (
	"context"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	gemaalv1 "github.com/truvity/gemaal/gen/gemaal/v1"
)

// GetMe returns the authenticated caller, so the console header can show who
// is signed in and whether operator affordances apply.
func (s *Service) GetMe(
	ctx context.Context,
	req *connect.Request[gemaalv1.GetMeRequest],
) (*connect.Response[gemaalv1.GetMeResponse], error) {
	caller, err := s.authenticate(ctx, req.Header())
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&gemaalv1.GetMeResponse{
		Subject: caller.Subject,
		Email:   caller.Email,
		Groups:  caller.Groups,
		Admin:   s.isAdmin(caller),
		Method:  caller.Method,
	}), nil
}

// GetVersion returns the running build's identity, for the console footer.
func (s *Service) GetVersion(
	_ context.Context,
	_ *connect.Request[gemaalv1.GetVersionRequest],
) (*connect.Response[gemaalv1.GetVersionResponse], error) {
	return connect.NewResponse(&gemaalv1.GetVersionResponse{Version: s.deps.Version}), nil
}

// History returns the sweep audit records for the console's Sweeps view.
// Read-only and derived from state the panel already showed to every
// authenticated viewer, so it authenticates but does not authorize.
func (s *Service) History(
	ctx context.Context,
	req *connect.Request[gemaalv1.HistoryRequest],
) (*connect.Response[gemaalv1.HistoryResponse], error) {
	if _, err := s.authenticate(ctx, req.Header()); err != nil {
		return nil, err
	}

	records := s.deps.History.List()
	out := make([]*gemaalv1.SweepHistoryRecord, 0, len(records))

	for i := range records {
		record := &records[i]

		row := &gemaalv1.SweepHistoryRecord{
			At:         timestamppb.New(record.At),
			DryRun:     record.DryRun,
			Source:     record.Source,
			Kept:       int32(record.Kept),          //nolint:gosec // small count
			Problems:   int32(len(record.Problems)), //nolint:gosec // small count
			QuietTicks: int32(record.QuietTicks),    //nolint:gosec // small count
		}

		for j := range record.Results {
			result := &record.Results[j]
			row.Details = append(row.Details, &gemaalv1.SweepHistoryDetail{
				Kind:     string(result.Action.Item.Kind),
				Target:   result.Action.Item.ID,
				Rule:     string(result.Action.Rule),
				Reason:   result.Action.Reason,
				Executed: result.Executed,
				Error:    result.Err,
			})
		}

		out = append(out, row)
	}

	return connect.NewResponse(&gemaalv1.HistoryResponse{Records: out}), nil
}
