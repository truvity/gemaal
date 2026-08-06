package engine_test

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/truvity/gemaal/pkg/config"
	"github.com/truvity/gemaal/pkg/engine"
)

// now is the fake clock every engine test pins.
var now = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

// fakeKube serves namespaces and release secrets from memory and records
// label patches.
type fakeKube struct {
	namespaces []engine.Namespace
	nsErr      error

	// secrets is keyed by namespace.
	secrets    map[string][]engine.ReleaseSecret
	secretsErr error

	labeled  []string // "<ns>/<secret> k=v,k=v"
	labelErr error
}

func (f *fakeKube) ListTierNamespaces(_ context.Context, _ string, _ []string) ([]engine.Namespace, error) {
	return f.namespaces, f.nsErr
}

func (f *fakeKube) ReleaseSecrets(_ context.Context, namespace string) ([]engine.ReleaseSecret, error) {
	if f.secretsErr != nil {
		return nil, f.secretsErr
	}

	return f.secrets[namespace], nil
}

func (f *fakeKube) LabelSecret(_ context.Context, namespace, secret string, labels map[string]string) error {
	if f.labelErr != nil {
		return f.labelErr
	}

	entry := namespace + "/" + secret
	for k, v := range labels {
		entry += " " + k + "=" + v
	}

	f.labeled = append(f.labeled, entry)

	return nil
}

// fakeHelm serves releases from memory and records uninstalls in order.
type fakeHelm struct {
	// releases is keyed by namespace.
	releases map[string][]engine.Release
	listErr  error

	uninstalled  []string // "<ns>/<release>"
	uninstallErr map[string]error
}

func (f *fakeHelm) ListReleases(_ context.Context, namespace string) ([]engine.Release, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}

	return f.releases[namespace], nil
}

func (f *fakeHelm) Uninstall(_ context.Context, namespace, release string) error {
	id := namespace + "/" + release

	if err := f.uninstallErr[id]; err != nil {
		return err
	}

	f.uninstalled = append(f.uninstalled, id)

	return nil
}

// fakeSSM serves parameters from memory and records deletions.
type fakeSSM struct {
	parameters []engine.Parameter
	listErr    error

	deleted   [][]string
	deleteErr error
}

func (f *fakeSSM) ListParameters(_ context.Context, _ string, visit func(engine.Parameter) error) error {
	if f.listErr != nil {
		return f.listErr
	}

	for _, p := range f.parameters {
		if err := visit(p); err != nil {
			return err
		}
	}

	return nil
}

func (f *fakeSSM) DeleteParameters(_ context.Context, names []string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}

	f.deleted = append(f.deleted, names)

	return nil
}

// fakeS3 serves objects from memory.
type fakeS3 struct {
	objects map[string][]engine.Object // by bucket

	deleted []string // "<bucket>/<prefix>"
}

func (f *fakeS3) ListObjects(_ context.Context, bucket string, visit func(engine.Object) error) error {
	for _, o := range f.objects[bucket] {
		if err := visit(o); err != nil {
			return err
		}
	}

	return nil
}

func (f *fakeS3) DeletePrefix(_ context.Context, bucket, prefix string) (int, error) {
	f.deleted = append(f.deleted, bucket+"/"+prefix)

	return 1, nil
}

// testConfig is the baseline: employee 14h / ci 24h, one /test/ root,
// 48h grace.
func testConfig(t *testing.T) *config.Config {
	t.Helper()

	cfg, err := config.Parse([]byte(`
tiers:
  employee:
    ttl: 14h
  ci:
    ttl: 24h
allowList:
  ssmRoots:
    - /test/
`))
	require.NoError(t, err)

	return cfg
}

// helmSecret builds a release Secret with the given ledger labels riding
// the helm-owned base labels.
func helmSecret(namespace, release string, version int, ledger map[string]string) engine.ReleaseSecret {
	labels := map[string]string{
		"owner":   "helm",
		"name":    release,
		"version": fmt.Sprint(version),
	}

	for k, v := range ledger {
		labels[k] = v
	}

	return engine.ReleaseSecret{
		Namespace: namespace,
		Name:      fmt.Sprintf("sh.helm.release.v1.%s.v%d", release, version),
		Release:   release,
		Version:   version,
		Labels:    labels,
	}
}

// newEngine wires an engine over the fakes with the pinned clock.
func newEngine(cfg *config.Config, kube *fakeKube, helm *fakeHelm, ssm *fakeSSM, s3 *fakeS3) *engine.Engine {
	e := &engine.Engine{
		Config: cfg,
		Kube:   kube,
		Helm:   helm,
		Now:    func() time.Time { return now },
		Logger: slog.New(slog.DiscardHandler),
	}

	if ssm != nil {
		e.SSM = ssm
	}

	if s3 != nil {
		e.S3 = s3
	}

	return e
}
