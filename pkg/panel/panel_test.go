package panel_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/gemaal/pkg/authn"
	"github.com/truvity/gemaal/pkg/config"
	"github.com/truvity/gemaal/pkg/engine"
	"github.com/truvity/gemaal/pkg/panel"
	"github.com/truvity/gemaal/pkg/service"
)

var now = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

type fakeKeeper struct {
	view           *engine.View
	stamped        []string
	decommissioned []string
}

func (f *fakeKeeper) View(context.Context) (*engine.View, error) { return f.view, nil }

func (f *fakeKeeper) Plan(context.Context, engine.Narrow) (*engine.Plan, error) {
	return &engine.Plan{}, nil
}

func (f *fakeKeeper) Apply(context.Context, *engine.Plan, bool, string) (*engine.SweepRecord, error) {
	return &engine.SweepRecord{}, nil
}

func (f *fakeKeeper) StampKeepUntil(_ context.Context, tenant engine.Tenant, until time.Time) error {
	f.stamped = append(f.stamped, tenant.String()+" until "+engine.FormatKeepUntil(until))

	return nil
}

func (f *fakeKeeper) Decommission(_ context.Context, namespace, release string, _ bool) (*engine.SweepRecord, error) {
	f.decommissioned = append(f.decommissioned, namespace+"/"+release)

	return &engine.SweepRecord{Source: "decommission"}, nil
}

// ownerJWT is a well-formed (unsigned) gateway-forwarded token — the
// header identity parses it for display, the fake authenticator accepts
// it for the mutations.
var ownerJWT = func() string {
	payload, _ := json.Marshal(map[string]any{
		"sub":    "123",
		"email":  "j.doe@example.com",
		"groups": []string{"emp:jdoe"},
	})

	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`)) +
		"." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}()

type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, authorization string) (authn.Identity, error) {
	if authorization != "Bearer "+ownerJWT {
		return authn.Identity{}, authn.ErrNoCredentials
	}

	return authn.Identity{
		Subject: "123",
		Email:   "j.doe@example.com",
		Groups:  []string{"emp:jdoe"},
		Method:  authn.MethodGatewayJWT,
	}, nil
}

func testView() *engine.View {
	return &engine.View{
		At:         now,
		Namespaces: []engine.Namespace{{Name: "emp-jdoe", Tier: "employee"}},
		Tenants: []engine.TenantState{
			{
				Tenant:       engine.Tenant{Namespace: "emp-jdoe", Release: "myapp"},
				Tier:         "employee",
				Ledger:       engine.Ledger{ExecutionID: "local-1", KeepUntil: now.Add(4 * time.Hour)},
				LastActivity: now.Add(-2 * time.Hour),
				TTL:          14 * time.Hour,
				TTLSource:    "tier-default",
			},
			{
				Tenant:       engine.Tenant{Namespace: "ci-truvity-bar", Release: "r1-a1"},
				Tier:         "ci",
				LastActivity: now.Add(-30 * time.Hour),
				TTL:          24 * time.Hour,
				TTLSource:    "tier-default",
			},
		},
	}
}

func newTestPanel(t *testing.T, keeper *fakeKeeper) (*httptest.Server, *engine.History) {
	t.Helper()

	cfg, err := config.Parse([]byte(`
tiers:
  employee:
    ttl: 14h
identity:
  emails:
    j.doe@example.com: jdoe
`))
	require.NoError(t, err)

	history := engine.NewHistory(10)

	svc := service.New(service.Deps{
		Config:  cfg,
		Logger:  slog.New(slog.DiscardHandler),
		Keeper:  keeper,
		Auth:    fakeAuth{},
		History: history,
		Now:     func() time.Time { return now },
	})

	ui, err := panel.New(cfg, slog.New(slog.DiscardHandler), svc, "test")
	require.NoError(t, err)

	mux := http.NewServeMux()
	ui.Register(mux)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server, history
}

// get fetches a page without following redirects.
func get(t *testing.T, server *httptest.Server, path string, header http.Header) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+path, http.NoBody)
	require.NoError(t, err)

	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	return resp
}

func post(t *testing.T, server *httptest.Server, path string, form url.Values, header http.Header) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		server.URL+path, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	return resp
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return string(raw)
}

func TestTenantsPage(t *testing.T) {
	server, _ := newTestPanel(t, &fakeKeeper{view: testView()})

	resp := get(t, server, "/", http.Header{"Authorization": {"Bearer " + ownerJWT}})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	html := body(t, resp)
	assert.Contains(t, html, "emp-jdoe/myapp")
	assert.Contains(t, html, "ci-truvity-bar/r1-a1")
	assert.Contains(t, html, "teardown", "the expired CI tenant shows its pending action")
	assert.Contains(t, html, "j.doe@example.com", "the forwarded identity renders in the header")
	assert.Contains(t, html, "Shadow mode", "the dry-run banner shows on a dry-run server")
	assert.Contains(t, html, `action="/tenants/checkout"`)
}

func TestSecurityHeaders(t *testing.T) {
	server, _ := newTestPanel(t, &fakeKeeper{view: testView()})

	resp := get(t, server, "/", nil)
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))

	csp := resp.Header.Get("Content-Security-Policy")
	assert.Contains(t, csp, "script-src 'none'", "no inline scripts, no scripts at all")
	assert.Contains(t, csp, "frame-ancestors 'none'")
}

func TestUnknownPathIs404(t *testing.T) {
	server, _ := newTestPanel(t, &fakeKeeper{view: testView()})

	resp := get(t, server, "/nope", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestSweepsPage(t *testing.T) {
	server, history := newTestPanel(t, &fakeKeeper{view: testView()})

	history.Add(engine.SweepRecord{
		At: now, DryRun: true, Source: "loop", Kept: 3,
		Results: []engine.ActionResult{{
			Action: engine.Action{
				Item: engine.Item{Kind: engine.KindHelmRelease, ID: "ci-truvity-bar/r1-a1"},
				Rule: engine.RuleTTLExpired, Reason: "idle 30h exceeds ttl 24h",
			},
		}},
	})

	resp := get(t, server, "/sweeps", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	html := body(t, resp)
	assert.Contains(t, html, "dry-run")
	assert.Contains(t, html, "ci-truvity-bar/r1-a1")
	assert.Contains(t, html, "ttl-expired")
}

func TestCheckoutPostFlow(t *testing.T) {
	keeper := &fakeKeeper{view: testView()}
	server, _ := newTestPanel(t, keeper)

	resp := post(t, server, "/tenants/checkout", url.Values{
		"namespace": {"emp-jdoe"}, "release": {"myapp"}, "duration": {"4h"},
	}, http.Header{
		"Authorization":  {"Bearer " + ownerJWT},
		"Sec-Fetch-Site": {"same-origin"},
	})

	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Location"), "msg=", "success redirects with a message")
	require.Len(t, keeper.stamped, 1)
	assert.Contains(t, keeper.stamped[0], "emp-jdoe/myapp")
}

func TestExtendPostFlow(t *testing.T) {
	keeper := &fakeKeeper{view: testView()}
	server, _ := newTestPanel(t, keeper)

	resp := post(t, server, "/tenants/extend", url.Values{
		"namespace": {"emp-jdoe"}, "release": {"myapp"}, "duration": {"8h"},
	}, http.Header{
		"Authorization":  {"Bearer " + ownerJWT},
		"Sec-Fetch-Site": {"same-origin"},
	})

	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Len(t, keeper.stamped, 1)
}

func TestPostWithoutIdentityRedirectsWithError(t *testing.T) {
	keeper := &fakeKeeper{view: testView()}
	server, _ := newTestPanel(t, keeper)

	resp := post(t, server, "/tenants/checkout", url.Values{
		"namespace": {"emp-jdoe"}, "release": {"myapp"}, "duration": {"4h"},
	}, http.Header{"Sec-Fetch-Site": {"same-origin"}})

	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Location"), "err=")
	assert.Empty(t, keeper.stamped)
}

func TestPostBadDurationRefused(t *testing.T) {
	keeper := &fakeKeeper{view: testView()}
	server, _ := newTestPanel(t, keeper)

	resp := post(t, server, "/tenants/checkout", url.Values{
		"namespace": {"emp-jdoe"}, "release": {"myapp"}, "duration": {"whenever"},
	}, http.Header{
		"Authorization":  {"Bearer " + ownerJWT},
		"Sec-Fetch-Site": {"same-origin"},
	})

	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Location"), "err=")
	assert.Empty(t, keeper.stamped)
}

func TestCrossSitePostRefused(t *testing.T) {
	// The gateway holds a session cookie, so a cross-site form post
	// would arrive authenticated — provenance is the whole question.
	keeper := &fakeKeeper{view: testView()}
	server, _ := newTestPanel(t, keeper)

	form := url.Values{"namespace": {"emp-jdoe"}, "release": {"myapp"}, "duration": {"4h"}}

	resp := post(t, server, "/tenants/checkout", form, http.Header{
		"Authorization":  {"Bearer " + ownerJWT},
		"Sec-Fetch-Site": {"cross-site"},
	})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	resp = post(t, server, "/tenants/checkout", form, http.Header{
		"Authorization": {"Bearer " + ownerJWT},
		"Origin":        {"https://evil.example.com"},
	})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	resp = post(t, server, "/tenants/checkout", form, http.Header{
		"Authorization": {"Bearer " + ownerJWT},
	})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "no provenance, no write")

	assert.Empty(t, keeper.stamped)
}

func TestSameOriginViaOriginHeader(t *testing.T) {
	keeper := &fakeKeeper{view: testView()}
	server, _ := newTestPanel(t, keeper)

	origin := server.URL // http://127.0.0.1:PORT — same host the request hits

	resp := post(t, server, "/tenants/checkout", url.Values{
		"namespace": {"emp-jdoe"}, "release": {"myapp"}, "duration": {"4h"},
	}, http.Header{
		"Authorization": {"Bearer " + ownerJWT},
		"Origin":        {origin},
	})

	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Len(t, keeper.stamped, 1)
}

func TestPostForeignNamespaceShowsDenial(t *testing.T) {
	// The service's own authorization answers the panel's post: jdoe
	// does not own ci-truvity-bar, and the refusal message reaches the
	// redirect.
	keeper := &fakeKeeper{view: testView()}
	server, _ := newTestPanel(t, keeper)

	resp := post(t, server, "/tenants/extend", url.Values{
		"namespace": {"ci-truvity-bar"}, "release": {"r1-a1"}, "duration": {"4h"},
	}, http.Header{
		"Authorization":  {"Bearer " + ownerJWT},
		"Sec-Fetch-Site": {"same-origin"},
	})

	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	location, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	assert.Contains(t, location.Query().Get("err"), "does not own")
	assert.Empty(t, keeper.stamped)
}

func TestAgeRendering(t *testing.T) {
	// The compact age formats, exercised through the page: minutes,
	// hours+minutes, whole days, days+hours.
	view := &engine.View{
		At: now,
		Tenants: []engine.TenantState{
			{Tenant: engine.Tenant{Namespace: "emp-a", Release: "r"}, Tier: "employee",
				LastActivity: now.Add(-45 * time.Minute), TTL: time.Hour},
			{Tenant: engine.Tenant{Namespace: "emp-b", Release: "r"}, Tier: "employee",
				LastActivity: now.Add(-(26*time.Hour + 30*time.Minute)), TTL: 100 * time.Hour},
			{Tenant: engine.Tenant{Namespace: "emp-c", Release: "r"}, Tier: "employee",
				LastActivity: now.Add(-96 * time.Hour), TTL: 200 * time.Hour},
			{Tenant: engine.Tenant{Namespace: "emp-d", Release: "r"}, Tier: "employee",
				LastActivity: now.Add(-100 * time.Hour), TTL: 200 * time.Hour},
			{Tenant: engine.Tenant{Namespace: "emp-e", Release: "r"}, Tier: "employee",
				LastActivity: now, TTL: 26 * time.Hour},
		},
	}

	server, _ := newTestPanel(t, &fakeKeeper{view: view})

	html := body(t, get(t, server, "/", nil))
	assert.Contains(t, html, "<td>45m</td>")
	assert.Contains(t, html, "<td>26h30m</td>")
	assert.Contains(t, html, "<td>4d</td>")
	assert.Contains(t, html, "<td>4d4h</td>")
	assert.Contains(t, html, "<td>0m</td>")
	assert.Contains(t, html, "<td>26h</td>", "a whole-hour TTL renders without minutes")
}

func TestTemplateEscapesTenantNames(t *testing.T) {
	// The console renders cluster-sourced strings; escaping is what
	// keeps a malicious release name inert.
	view := testView()
	view.Tenants[0].Ledger.ExecutionID = `<script>alert(1)</script>`

	server, _ := newTestPanel(t, &fakeKeeper{view: view})

	html := body(t, get(t, server, "/", nil))
	assert.NotContains(t, html, "<script>alert(1)</script>")
	assert.Contains(t, html, "&lt;script&gt;")
}

// TestPanelDestroyRequiresTheConfirmBox: the CSP forbids scripts, so the
// destructive guard is a required checkbox — and it must hold server-side
// too, because a form can be forged without the UI.
func TestPanelDestroyRequiresTheConfirmBox(t *testing.T) {
	keeper := &fakeKeeper{view: testView()}
	server, _ := newTestPanel(t, keeper)

	resp := post(t, server, "/tenants/decommission", url.Values{
		"namespace": {"emp-jdoe"}, "release": {"myapp"},
	}, http.Header{
		"Authorization":  {"Bearer " + ownerJWT},
		"Sec-Fetch-Site": {"same-origin"},
	})

	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Location"), "confirmation+box",
		"an unticked confirm must bounce with the banner, not destroy")
}

// TestPanelDestroyPostsThroughTheService: the button is the service's
// own Decommission with the forwarded identity — same authn, same authz.
func TestPanelDestroyPostsThroughTheService(t *testing.T) {
	keeper := &fakeKeeper{view: testView()}
	server, _ := newTestPanel(t, keeper)

	resp := post(t, server, "/tenants/decommission", url.Values{
		"namespace": {"emp-jdoe"}, "release": {"myapp"}, "confirm": {"yes"},
	}, http.Header{
		"Authorization":  {"Bearer " + ownerJWT},
		"Sec-Fetch-Site": {"same-origin"},
	})

	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Location"), "destroy", "the banner reports the outcome")
	assert.Equal(t, []string{"emp-jdoe/myapp"}, keeper.decommissioned,
		"the engine must actually have been asked — a banner alone proves nothing")
}

// TestPanelDestroyRendersOnTheTenantsPage: the form ships in the row.
func TestPanelDestroyRendersOnTheTenantsPage(t *testing.T) {
	server, _ := newTestPanel(t, &fakeKeeper{view: testView()})

	resp := get(t, server, "/", http.Header{"Authorization": {"Bearer " + ownerJWT}})
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	html := string(body)
	assert.Contains(t, html, `action="/tenants/decommission"`)
	assert.Contains(t, html, `name="confirm"`, "the pure-HTML guard must ship with the form")
}
