package authn_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/truvity/gemaal/pkg/authn"
)

func TestUserinfoEnrichFillsEmailAndRoles(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"sub": "379486258717560197",
			"email": "person@example.com",
			"urn:zitadel:iam:org:project:roles": {
				"ci-truvity-bar:deployer": {"1": "org.example.com"},
				"platform:admin": {"1": "org.example.com"}
			}
		}`))
	}))
	defer server.Close()

	e := &authn.UserinfoEnricher{URL: server.URL, Client: server.Client()}

	got := e.Enrich(t.Context(), "tok", authn.Identity{Subject: "379486258717560197"})

	assert.Equal(t, "person@example.com", got.Email)
	assert.Equal(t, []string{"ci-truvity-bar:deployer", "platform:admin"}, got.Groups)
}

func TestUserinfoEnrichKeepsExistingEmailAndGroups(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"email": "other@example.com"}`))
	}))
	defer server.Close()

	e := &authn.UserinfoEnricher{URL: server.URL, Client: server.Client()}

	got := e.Enrich(t.Context(), "tok", authn.Identity{Subject: "x", Email: "keep@example.com", Groups: []string{"g"}})

	assert.Equal(t, "keep@example.com", got.Email)
	assert.Equal(t, []string{"g"}, got.Groups)
}

func TestUserinfoEnrichDegradesOnFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	e := &authn.UserinfoEnricher{URL: server.URL, Client: server.Client()}

	in := authn.Identity{Subject: "379486258717560197"}
	got := e.Enrich(t.Context(), "tok", in)

	assert.Equal(t, in, got)
}
