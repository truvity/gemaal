package identity

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gemaalv1 "github.com/truvity/gemaal/gen/gemaal/v1"
	"github.com/truvity/gemaal/gen/gemaal/v1/gemaalv1connect"
)

// stubGemaalServer answers the Resolve RPC from a fixed map — the shape
// of the deployed service, without a deployment.
type stubGemaalServer struct {
	gemaalv1connect.UnimplementedGemaalServiceHandler

	slugs map[string]string
	delay time.Duration
	calls int
}

func (s *stubGemaalServer) Resolve(
	ctx context.Context,
	req *connect.Request[gemaalv1.ResolveRequest],
) (*connect.Response[gemaalv1.ResolveResponse], error) {
	s.calls++

	if s.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, connect.NewError(connect.CodeDeadlineExceeded, ctx.Err())
		case <-time.After(s.delay):
		}
	}

	slug, ok := s.slugs[req.Msg.GetEmail()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("email %q has no slug in the identity map", req.Msg.GetEmail()))
	}

	return connect.NewResponse(&gemaalv1.ResolveResponse{Slug: slug, Namespace: "emp-" + slug}), nil
}

// startStub serves the stub over a real HTTP listener and cleans it up.
func startStub(t *testing.T, stub *stubGemaalServer) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	path, handler := gemaalv1connect.NewGemaalServiceHandler(stub)
	mux.Handle(path, handler)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv
}

func TestResolveClient(t *testing.T) {
	t.Run("resolves a mapped email over the RPC", func(t *testing.T) {
		stub := &stubGemaalServer{slugs: map[string]string{"j.doe@example.com": "jdoe"}}
		srv := startStub(t, stub)

		slug, err := ResolveClient{Server: srv.URL}.Resolve(context.Background(), "j.doe@example.com")
		require.NoError(t, err)
		assert.Equal(t, "jdoe", slug)
		assert.Equal(t, 1, stub.calls)
	})

	t.Run("an unmapped email surfaces the server's refusal, with the address", func(t *testing.T) {
		stub := &stubGemaalServer{}
		srv := startStub(t, stub)

		_, err := ResolveClient{Server: srv.URL}.Resolve(context.Background(), "ghost@example.com")
		require.Error(t, err)
		assert.Contains(t, err.Error(), srv.URL, "the operator must see WHICH authority refused")
		assert.Contains(t, err.Error(), "ghost@example.com")
		assert.Contains(t, err.Error(), "no slug")
	})

	t.Run("no server address is a configuration error, not a call", func(t *testing.T) {
		_, err := ResolveClient{}.Resolve(context.Background(), "j.doe@example.com")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no gemaal server address configured")
	})

	t.Run("a slow authority fails within the timeout instead of hanging", func(t *testing.T) {
		stub := &stubGemaalServer{
			slugs: map[string]string{"j.doe@example.com": "jdoe"},
			delay: time.Second,
		}
		srv := startStub(t, stub)

		start := time.Now()
		_, err := ResolveClient{Server: srv.URL, Timeout: 50 * time.Millisecond}.
			Resolve(context.Background(), "j.doe@example.com")
		require.Error(t, err)
		assert.Less(t, time.Since(start), time.Second, "the client's own timeout must bound the call")
	})

	t.Run("an unreachable server reports the address", func(t *testing.T) {
		_, err := ResolveClient{
			Server:  "http://127.0.0.1:1",
			Timeout: time.Second,
		}.Resolve(context.Background(), "j.doe@example.com")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "http://127.0.0.1:1")
	})
}
