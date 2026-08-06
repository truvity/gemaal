// Package engine is the housekeeping engine: the pump itself.
//
// It is the port of bar's janitor prototype (bar#486, pkg/janitor)
// adapted to the label-ledger model of docs/design.md. What carried
// over: the plan/apply split (a Plan is inert; Apply is the only thing
// that acts on one), exec-based helm/kubectl ports (no client-go), the
// orphan-plus-grace artifact rule, the failure policy (release truth is
// fatal, artifact listings are not), the keep-record auditability, and
// the re-authorization of every action before anything is deleted. What
// changed: tier selection moved from name prefixes to the namespace
// tier LABEL, the "standing installs are never swept" special case gave
// way to the uniform TTL (from last activity + keep-until), and the
// ledger — ttl / keep-until / execution-id labels on the release
// Secrets — replaced the prototype's config-only clocks.
//
// Everything that touches the outside world is an interface, so the
// garbage rules are unit-tested with injected fakes and a fake clock.
package engine

import (
	"log/slog"
	"time"

	"github.com/truvity/gemaal/pkg/config"
)

// Engine wires the housekeeping rules to their collaborators.
type Engine struct {
	// Config is the validated service configuration: the allow-list, the
	// tiers, the ledger label domain.
	Config *config.Config

	// Kube lists tier-labeled namespaces and reads/patches the ledger
	// labels on release Secrets.
	Kube KubeClient

	// Helm is the release truth: listing and uninstalling.
	Helm HelmClient

	// SSM enumerates and deletes under the allow-listed /test/ roots.
	// Nil skips SSM (recorded as a problem when roots are configured).
	SSM SSMClient

	// S3 enumerates and deletes tenant prefixes in the allow-listed test
	// buckets. Nil skips S3 — the production wiring passes nil until the
	// shared test bucket exists (the target is stubbed off, and the plan
	// says so rather than staying silent).
	S3 S3Client

	// Now is the clock; time.Now when nil.
	Now func() time.Time

	// Logger receives the structured deletion records.
	Logger *slog.Logger
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}

	return time.Now()
}

func (e *Engine) log() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}

	return slog.New(slog.DiscardHandler)
}
