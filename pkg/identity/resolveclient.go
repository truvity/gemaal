package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"

	gemaalv1 "github.com/truvity/gemaal/gen/gemaal/v1"
	"github.com/truvity/gemaal/gen/gemaal/v1/gemaalv1connect"
)

// DefaultResolveTimeout bounds one Resolve RPC. Resolution happens on
// the interactive path (whoami, install) — a slow authority should fail
// fast and say where it was asked, not hang a laptop command.
const DefaultResolveTimeout = 10 * time.Second

// ResolveClient resolves email→slug by asking a deployed gemaal
// service's Resolve RPC — the resolution authority, serving the
// deployer-rendered personnel map. It fills the Resolver seam the
// interim resolvers (MapResolver from a committed gemaal.yaml map,
// KubectlGroupsResolver from the caller's token groups) stand in for.
//
// Like KubectlGroupsResolver, failure is a hard error, never a
// fall-through: resolution was already reached, and failing loudly beats
// resolving as somebody else.
type ResolveClient struct {
	// Server is the base URL of the gemaal service, e.g.
	// "http://gemaal.gemaal-system.svc:8080" — scheme included, the shape
	// a ConnectRPC client needs.
	Server string

	// HTTPClient overrides the transport (the seam for tests and for
	// callers with in-cluster TLS material); http.DefaultClient when nil.
	HTTPClient connect.HTTPClient

	// Timeout bounds each call; DefaultResolveTimeout when zero.
	Timeout time.Duration
}

// Resolve implements Resolver.
func (r ResolveClient) Resolve(ctx context.Context, email string) (string, error) {
	if r.Server == "" {
		return "", errors.New("resolve client: no gemaal server address configured")
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()

	client := gemaalv1connect.NewGemaalServiceClient(r.httpClient(), r.Server)

	resp, err := client.Resolve(ctx, connect.NewRequest(&gemaalv1.ResolveRequest{Email: email}))
	if err != nil {
		return "", fmt.Errorf("gemaal service at %s could not resolve %s: %w", r.Server, email, err)
	}

	slug := resp.Msg.GetSlug()
	if slug == "" {
		return "", fmt.Errorf("gemaal service at %s returned an empty slug for %s", r.Server, email)
	}

	return slug, nil
}

func (r ResolveClient) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}

	return DefaultResolveTimeout
}

func (r ResolveClient) httpClient() connect.HTTPClient {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}

	return http.DefaultClient
}
