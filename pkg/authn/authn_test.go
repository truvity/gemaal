package authn_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/gemaal/pkg/authn"
)

// jwt builds an unsigned-but-well-formed JWT carrying the claims.
func jwt(t *testing.T, claims map[string]any) string {
	t.Helper()

	payload, err := json.Marshal(claims)
	require.NoError(t, err)

	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	body := base64.RawURLEncoding.EncodeToString(payload)

	return head + "." + body + ".signature"
}

// stubReviewer scripts TokenReview.
type stubReviewer struct {
	identity      authn.Identity
	authenticated bool
	err           error
	reviewed      []string
}

func (s *stubReviewer) Review(_ context.Context, token string) (authn.Identity, bool, error) {
	s.reviewed = append(s.reviewed, token)

	return s.identity, s.authenticated, s.err
}

func TestNoBearerTokenRefused(t *testing.T) {
	auth := &authn.Authenticator{GroupsClaim: "groups"}

	for _, header := range []string{"", "Basic dXNlcg==", "Bearer ", "Bearer"} {
		_, err := auth.Authenticate(context.Background(), header)
		require.ErrorIs(t, err, authn.ErrNoCredentials, "header %q", header)
	}
}

func TestGatewayJWTClaims(t *testing.T) {
	auth := &authn.Authenticator{GroupsClaim: "groups"}

	token := jwt(t, map[string]any{
		"sub":    "123",
		"email":  "j.doe@example.com",
		"groups": []any{"emp:jdoe", "cluster-devel:cluster:viewer"},
	})

	identity, err := auth.Authenticate(context.Background(), "Bearer "+token)
	require.NoError(t, err)

	assert.Equal(t, "123", identity.Subject)
	assert.Equal(t, "j.doe@example.com", identity.Email)
	assert.Equal(t, []string{"emp:jdoe", "cluster-devel:cluster:viewer"}, identity.Groups)
	assert.Equal(t, authn.MethodGatewayJWT, identity.Method)
}

func TestGatewayJWTSingleStringGroup(t *testing.T) {
	// Providers disagree about whether one group is a string or an array.
	auth := &authn.Authenticator{GroupsClaim: "groups"}

	token := jwt(t, map[string]any{"email": "a@b.c", "groups": "emp:jdoe"})

	identity, err := auth.Authenticate(context.Background(), "Bearer "+token)
	require.NoError(t, err)
	assert.Equal(t, []string{"emp:jdoe"}, identity.Groups)
	assert.Equal(t, "a@b.c", identity.Subject, "email backfills a missing sub")
}

func TestGatewayJWTGarbageRefused(t *testing.T) {
	auth := &authn.Authenticator{GroupsClaim: "groups"}

	for _, token := range []string{"not-a-jwt", "a.!!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte("[]")) + ".c"} {
		_, err := auth.Authenticate(context.Background(), "Bearer "+token)
		require.Error(t, err, "token %q", token)
	}

	_, err := auth.Authenticate(context.Background(), "Bearer "+jwt(t, map[string]any{}))
	require.ErrorContains(t, err, "no identity claims")
}

func TestTokenReviewWinsForWorkloads(t *testing.T) {
	reviewer := &stubReviewer{
		identity: authn.Identity{
			Subject: "system:serviceaccount:ci-truvity-bar:tester",
			Groups:  []string{"system:serviceaccounts"},
			Method:  authn.MethodTokenReview,
		},
		authenticated: true,
	}
	auth := &authn.Authenticator{Reviewer: reviewer, GroupsClaim: "groups"}

	identity, err := auth.Authenticate(context.Background(), "Bearer sa-token")
	require.NoError(t, err)
	assert.Equal(t, "system:serviceaccount:ci-truvity-bar:tester", identity.Subject)
	assert.Equal(t, authn.MethodTokenReview, identity.Method)
	assert.Equal(t, []string{"sa-token"}, reviewer.reviewed)
}

func TestUnrecognizedTokenFallsThroughToClaims(t *testing.T) {
	reviewer := &stubReviewer{authenticated: false}
	auth := &authn.Authenticator{Reviewer: reviewer, GroupsClaim: "groups"}

	token := jwt(t, map[string]any{"email": "j.doe@example.com"})

	identity, err := auth.Authenticate(context.Background(), "Bearer "+token)
	require.NoError(t, err)
	assert.Equal(t, authn.MethodGatewayJWT, identity.Method)
	assert.Equal(t, "j.doe@example.com", identity.Email)
}

func TestBrokenReviewerFailsClosed(t *testing.T) {
	// An unreachable authority is not a license to downgrade to the
	// weaker parse.
	reviewer := &stubReviewer{err: errors.New("apiserver down")}
	auth := &authn.Authenticator{Reviewer: reviewer, GroupsClaim: "groups"}

	_, err := auth.Authenticate(context.Background(), "Bearer "+jwt(t, map[string]any{"email": "a@b.c"}))
	require.ErrorContains(t, err, "apiserver down")
}

func TestIdentityInAny(t *testing.T) {
	identity := authn.Identity{Groups: []string{"a", "b"}}

	assert.True(t, identity.InAny([]string{"x", "b"}))
	assert.False(t, identity.InAny([]string{"x", "y"}))
	assert.False(t, identity.InAny(nil))
	assert.False(t, authn.Identity{}.InAny([]string{""}), "an empty admin group must never match")
}

func TestKubeTokenReviewer(t *testing.T) {
	var got struct {
		Spec struct {
			Token     string   `json:"token"`
			Audiences []string `json:"audiences"`
		} `json:"spec"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/apis/authentication.k8s.io/v1/tokenreviews", r.URL.Path)
		require.Equal(t, "Bearer service-token", r.Header.Get("Authorization"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"status": {
				"authenticated": true,
				"user": {"username": "system:serviceaccount:ci-truvity-bar:tester", "groups": ["system:serviceaccounts"]}
			}
		}`))
	}))
	t.Cleanup(server.Close)

	reviewer := &authn.KubeTokenReviewer{
		BaseURL:   server.URL,
		Token:     "service-token",
		Client:    server.Client(),
		Audiences: []string{"gemaal"},
	}

	identity, ok, err := reviewer.Review(context.Background(), "the-token")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "system:serviceaccount:ci-truvity-bar:tester", identity.Subject)
	assert.Equal(t, []string{"system:serviceaccounts"}, identity.Groups)
	assert.Equal(t, authn.MethodTokenReview, identity.Method)

	assert.Equal(t, "the-token", got.Spec.Token)
	assert.Equal(t, []string{"gemaal"}, got.Spec.Audiences)
}

func TestKubeTokenReviewerUnauthenticated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status": {"authenticated": false, "error": "token expired"}}`))
	}))
	t.Cleanup(server.Close)

	reviewer := &authn.KubeTokenReviewer{BaseURL: server.URL, Token: "t", Client: server.Client()}

	_, ok, err := reviewer.Review(context.Background(), "stale")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestKubeTokenReviewerAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	reviewer := &authn.KubeTokenReviewer{BaseURL: server.URL, Token: "t", Client: server.Client()}

	_, _, err := reviewer.Review(context.Background(), "any")
	require.ErrorContains(t, err, "unexpected status")
}

func TestNewInClusterOutsideCluster(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	_, err := authn.NewInClusterTokenReviewer(nil)
	require.ErrorIs(t, err, authn.ErrNotInCluster)
}
