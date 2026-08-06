// Package panel is the gemaal web console: server-rendered pages over
// the same service the RPCs drive — the github-roster UI stack
// (html/template, embedded assets, no SPA, no front-end build step, no
// inline scripts).
//
// Authentication is the gateway's job (Envoy SecurityPolicy OIDC — the
// expose-a-ui pattern): the browser session, the login redirect and the
// JWKS validation all happen before a request arrives here, and the
// panel consumes the forwarded identity (roster's header: the access
// token in Authorization). Mutations POST into the service's own
// Checkout/Extend handlers with that same header, so the panel adds no
// second authorization path — the RPC's rules are the only rules.
package panel

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	gemaalv1 "github.com/truvity/gemaal/gen/gemaal/v1"
	"github.com/truvity/gemaal/pkg/authn"
	"github.com/truvity/gemaal/pkg/config"
	"github.com/truvity/gemaal/pkg/service"
)

//go:embed templates/*.html
var templateFS embed.FS

const layoutFile = "templates/layout.html"

// Panel serves the console pages.
type Panel struct {
	cfg     *config.Config
	log     *slog.Logger
	svc     *service.Service
	version string
	pages   map[string]*template.Template

	// identity parses the forwarded JWT for the header line — display
	// only; authorization stays with the RPCs.
	identity *authn.Authenticator
}

// New builds the panel, parsing the embedded templates at startup — a
// broken template must not reach production as a 500 on the one page
// nobody opened during the rollout.
func New(cfg *config.Config, log *slog.Logger, svc *service.Service, version string) (*Panel, error) {
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}

	pages := make(map[string]*template.Template, len(files))

	for _, file := range files {
		if file == layoutFile {
			continue
		}

		tmpl, err := template.ParseFS(templateFS, layoutFile, file)
		if err != nil {
			return nil, fmt.Errorf("parse template %q: %w", file, err)
		}

		pages[strings.TrimSuffix(path.Base(file), ".html")] = tmpl
	}

	if len(pages) == 0 {
		return nil, fmt.Errorf("no page templates found")
	}

	return &Panel{
		cfg:      cfg,
		log:      log,
		svc:      svc,
		version:  version,
		pages:    pages,
		identity: &authn.Authenticator{GroupsClaim: cfg.Authz.GroupsClaim},
	}, nil
}

// Register mounts the console on a mux. Go 1.22 patterns: "/{$}" keeps
// the tenants page off every unknown path (those 404).
func (p *Panel) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", p.harden(p.handleTenants))
	mux.HandleFunc("GET /sweeps", p.harden(p.handleSweeps))
	mux.HandleFunc("POST /tenants/checkout", p.harden(p.requireSameOrigin(p.handleHold("checkout"))))
	mux.HandleFunc("POST /tenants/extend", p.harden(p.requireSameOrigin(p.handleHold("extend"))))
}

// harden sets the response-hardening headers on every console page. The
// gateway authenticates; these bound what a rendered page can do. No
// scripts exist, so script-src is 'none'; the one inline style block in
// the layout needs style-src unsafe-inline (the roster trade, and the
// console serves no user-authored content — template escaping is what
// keeps the join safe, and that is tested).
func (p *Panel) harden(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; style-src 'unsafe-inline'; script-src 'none'; frame-ancestors 'none'")

		next(w, r)
	}
}

// requireSameOrigin rejects a state-changing request whose browser
// declares cross-site provenance — the stateless CSRF guard, ported
// from roster. It matters because the gateway holds a SESSION COOKIE: a
// form posted from another site would arrive authenticated.
func (p *Panel) requireSameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
			// "none" is a user-initiated navigation; browsers never
			// produce it for a scripted cross-site post.
			if site == "same-origin" || site == "none" {
				next(w, r)

				return
			}

			p.renderError(w, r, http.StatusForbidden, "cross-site request refused")

			return
		}

		origin := r.Header.Get("Origin")
		if origin == "" {
			p.renderError(w, r, http.StatusForbidden, "request declares no provenance (no Origin, no Sec-Fetch-Site)")

			return
		}

		parsed, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(parsed.Host, r.Host) {
			p.renderError(w, r, http.StatusForbidden, "cross-site request refused")

			return
		}

		next(w, r)
	}
}

// page is what every template receives.
type page struct {
	Title   string
	Nav     string
	Who     string
	Version string
	DryRun  bool
	Data    any
}

type errorData struct {
	Status  int
	Message string
}

type tenantRow struct {
	Namespace   string
	Release     string
	Tier        string
	Age         string
	TTL         string
	KeepUntil   string
	ExecutionID string
	Pending     string
	Expired     bool
}

type tenantsData struct {
	Rows    []tenantRow
	Message string
	Problem string
}

type sweepRow struct {
	At       string
	Mode     string
	Source   string
	Actions  int
	Executed int
	Failed   int
	Kept     int
	Problems int
	Details  []sweepDetail
}

type sweepDetail struct {
	Kind     string
	Target   string
	Rule     string
	Reason   string
	Executed bool
	Err      string
}

func (p *Panel) handleTenants(w http.ResponseWriter, r *http.Request) {
	resp, err := p.svc.ListTenants(r.Context(), connect.NewRequest(&gemaalv1.ListTenantsRequest{}))
	if err != nil {
		p.log.ErrorContext(r.Context(), "panel: list tenants failed", slog.Any("error", err))
		p.renderError(w, r, http.StatusInternalServerError, "could not build the tenant view")

		return
	}

	data := tenantsData{
		Message: r.URL.Query().Get("msg"),
		Problem: r.URL.Query().Get("err"),
	}

	for _, t := range resp.Msg.GetTenants() {
		row := tenantRow{
			Namespace:   t.GetNamespace(),
			Release:     t.GetRelease(),
			Tier:        t.GetTier(),
			Age:         humanDuration(t.GetAge().AsDuration()),
			TTL:         humanDuration(t.GetTtl().AsDuration()),
			KeepUntil:   "—",
			ExecutionID: t.GetExecutionId(),
			Pending:     "none",
		}

		if t.GetKeepUntil() != nil {
			row.KeepUntil = t.GetKeepUntil().AsTime().UTC().Format("2006-01-02 15:04 UTC")
		}

		if t.GetPendingAction() == gemaalv1.ActionKind_ACTION_KIND_TEARDOWN {
			row.Pending = "teardown"
			row.Expired = true
		}

		data.Rows = append(data.Rows, row)
	}

	p.render(w, r, http.StatusOK, "tenants", page{Title: "Tenants", Nav: "tenants", Data: data})
}

func (p *Panel) handleSweeps(w http.ResponseWriter, r *http.Request) {
	var rows []sweepRow

	for _, record := range p.svc.History().List() {
		row := sweepRow{
			At:       record.At.UTC().Format("2006-01-02 15:04:05 UTC"),
			Mode:     "executed",
			Source:   record.Source,
			Actions:  len(record.Results),
			Kept:     record.Kept,
			Problems: len(record.Problems),
		}

		if record.DryRun {
			row.Mode = "dry-run"
		}

		for i := range record.Results {
			result := &record.Results[i]

			if result.Executed {
				row.Executed++
			}

			if result.Err != "" {
				row.Failed++
			}

			row.Details = append(row.Details, sweepDetail{
				Kind:     string(result.Action.Item.Kind),
				Target:   result.Action.Item.ID,
				Rule:     string(result.Action.Rule),
				Reason:   result.Action.Reason,
				Executed: result.Executed,
				Err:      result.Err,
			})
		}

		rows = append(rows, row)
	}

	p.render(w, r, http.StatusOK, "sweeps", page{Title: "Sweeps", Nav: "sweeps", Data: rows})
}

// handleHold posts the form into the service's own Checkout/Extend —
// same handler, same authn, same authz, with the browser's forwarded
// Authorization header riding along.
func (p *Panel) handleHold(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			p.renderError(w, r, http.StatusBadRequest, "unreadable form")

			return
		}

		namespace := r.PostFormValue("namespace")
		release := r.PostFormValue("release")

		duration, err := time.ParseDuration(r.PostFormValue("duration"))
		if err != nil || duration <= 0 {
			p.redirect(w, r, "", "duration must be a positive Go duration (e.g. 8h)")

			return
		}

		until, err := p.callHold(r.Context(), kind, r.Header.Get("Authorization"), namespace, release, duration)
		if err != nil {
			p.redirect(w, r, "", holdError(err))

			return
		}

		p.redirect(w, r, fmt.Sprintf("%s %s/%s held until %s",
			kind, namespace, release, until.UTC().Format("2006-01-02 15:04 UTC")), "")
	}
}

// callHold speaks to the service in-process.
func (p *Panel) callHold(
	ctx context.Context,
	kind, authorization, namespace, release string,
	duration time.Duration,
) (time.Time, error) {
	if kind == "extend" {
		req := connect.NewRequest(&gemaalv1.ExtendRequest{Namespace: namespace, Release: release})
		req.Msg.Duration = durationpb.New(duration)
		req.Header().Set("Authorization", authorization)

		resp, err := p.svc.Extend(ctx, req)
		if err != nil {
			return time.Time{}, err
		}

		return resp.Msg.GetTenant().GetKeepUntil().AsTime(), nil
	}

	req := connect.NewRequest(&gemaalv1.CheckoutRequest{Namespace: namespace, Release: release})
	req.Msg.Duration = durationpb.New(duration)
	req.Header().Set("Authorization", authorization)

	resp, err := p.svc.Checkout(ctx, req)
	if err != nil {
		return time.Time{}, err
	}

	return resp.Msg.GetTenant().GetKeepUntil().AsTime(), nil
}

// holdError renders a connect error for the console banner.
func holdError(err error) string {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return "the request failed"
	}

	switch cerr.Code() {
	case connect.CodeUnauthenticated:
		return "not signed in (the gateway forwarded no usable identity)"
	case connect.CodePermissionDenied, connect.CodeNotFound, connect.CodeInvalidArgument:
		return cerr.Message()
	default:
		return "the request failed"
	}
}

func (p *Panel) redirect(w http.ResponseWriter, r *http.Request, msg, problem string) {
	target := "/"

	q := url.Values{}
	if msg != "" {
		q.Set("msg", msg)
	}

	if problem != "" {
		q.Set("err", problem)
	}

	if len(q) > 0 {
		target += "?" + q.Encode()
	}

	http.Redirect(w, r, target, http.StatusSeeOther)
}

// render writes a page. The template executes into a buffer first, so a
// template error becomes a clean 500 rather than a half-written page.
func (p *Panel) render(w http.ResponseWriter, r *http.Request, status int, name string, pg page) {
	tmpl, ok := p.pages[name]
	if !ok {
		p.renderError(w, r, http.StatusInternalServerError, "unknown page")

		return
	}

	pg.Version = p.version
	pg.DryRun = *p.cfg.DryRun
	pg.Who = p.who(r)

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout", pg); err != nil {
		p.log.ErrorContext(r.Context(), "panel: render failed", slog.String("page", name), slog.Any("error", err))
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (p *Panel) renderError(w http.ResponseWriter, r *http.Request, status int, message string) {
	p.render(w, r, status, "error", page{
		Title: "Error",
		Data:  errorData{Status: status, Message: message},
	})
}

// who renders the header identity from the forwarded token — display
// only, best-effort, anonymous on anything unparseable.
func (p *Panel) who(r *http.Request) string {
	identity, err := p.identity.Authenticate(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		return "anonymous"
	}

	if identity.Email != "" {
		return identity.Email
	}

	return identity.Subject
}

// humanDuration renders an age compactly: "3d4h", "26h", "45m".
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "0m"
	}

	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}

	hours := int(d.Hours())
	if hours < 48 {
		if m := int(d.Minutes()) % 60; m > 0 {
			return fmt.Sprintf("%dh%dm", hours, m)
		}

		return fmt.Sprintf("%dh", hours)
	}

	days := hours / 24
	if rest := hours % 24; rest > 0 {
		return fmt.Sprintf("%dd%dh", days, rest)
	}

	return fmt.Sprintf("%dd", days)
}
