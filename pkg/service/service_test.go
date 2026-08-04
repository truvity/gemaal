package service_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	gemaalv1 "github.com/truvity/gemaal/gen/gemaal/v1"
	"github.com/truvity/gemaal/gen/gemaal/v1/gemaalv1connect"
	"github.com/truvity/gemaal/pkg/config"
	"github.com/truvity/gemaal/pkg/service"
)

func newTestClient(t *testing.T) gemaalv1connect.GemaalServiceClient {
	t.Helper()

	cfg, err := config.Parse([]byte("{}"))
	require.NoError(t, err)

	svc := service.New(cfg, slog.New(slog.DiscardHandler))

	mux := http.NewServeMux()
	path, handler := svc.Handler()
	mux.Handle(path, handler)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return gemaalv1connect.NewGemaalServiceClient(server.Client(), server.URL)
}

func TestPlanReturnsEmptyPlan(t *testing.T) {
	client := newTestClient(t)

	resp, err := client.Plan(context.Background(), connect.NewRequest(&gemaalv1.PlanRequest{}))
	require.NoError(t, err)

	assert.Empty(t, resp.Msg.GetActions())
	assert.NotNil(t, resp.Msg.GetComputedAt())
}

func TestStubsReturnUnimplemented(t *testing.T) {
	client := newTestClient(t)
	ctx := context.Background()

	_, err := client.ListTenants(ctx, connect.NewRequest(&gemaalv1.ListTenantsRequest{}))
	assert.Equal(t, connect.CodeUnimplemented, connect.CodeOf(err))

	_, err = client.Checkout(ctx, connect.NewRequest(&gemaalv1.CheckoutRequest{}))
	assert.Equal(t, connect.CodeUnimplemented, connect.CodeOf(err))

	_, err = client.Extend(ctx, connect.NewRequest(&gemaalv1.ExtendRequest{}))
	assert.Equal(t, connect.CodeUnimplemented, connect.CodeOf(err))

	_, err = client.Sweep(ctx, connect.NewRequest(&gemaalv1.SweepRequest{}))
	assert.Equal(t, connect.CodeUnimplemented, connect.CodeOf(err))

	_, err = client.Resolve(ctx, connect.NewRequest(&gemaalv1.ResolveRequest{}))
	assert.Equal(t, connect.CodeUnimplemented, connect.CodeOf(err))
}

func TestProtoRoundTrip(t *testing.T) {
	original := &gemaalv1.PlanResponse{
		Actions: []*gemaalv1.PlannedAction{
			{
				Kind:      gemaalv1.ActionKind_ACTION_KIND_TEARDOWN,
				Namespace: "ci-myorg-myapp",
				Release:   "r123-a1",
				Target:    "helm release ci-myorg-myapp/r123-a1",
				Rule:      "ttl-expired",
				Reason:    "age 26h exceeds ttl 24h (tier ci default); no keep-until",
			},
		},
	}

	raw, err := proto.Marshal(original)
	require.NoError(t, err)

	decoded := &gemaalv1.PlanResponse{}
	require.NoError(t, proto.Unmarshal(raw, decoded))

	assert.True(t, proto.Equal(original, decoded))
	require.Len(t, decoded.GetActions(), 1)
	assert.Equal(t, "ttl-expired", decoded.GetActions()[0].GetRule())
}

func TestTenantDurationRoundTrip(t *testing.T) {
	original := &gemaalv1.Tenant{
		Namespace: "emp-jdoe",
		Release:   "myapp",
		Backend:   "helm",
		Tier:      "emp",
		Ttl:       durationpb.New(14 * time.Hour),
	}

	raw, err := proto.Marshal(original)
	require.NoError(t, err)

	decoded := &gemaalv1.Tenant{}
	require.NoError(t, proto.Unmarshal(raw, decoded))

	assert.Equal(t, 14*time.Hour, decoded.GetTtl().AsDuration())
}
