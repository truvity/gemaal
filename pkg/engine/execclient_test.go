package engine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/gemaal/pkg/engine"
)

// stubRunner scripts Output by argv prefix, newest rule first, and
// records every call for argv assertions.
type stubRunner struct {
	rules []struct {
		prefix string
		out    string
		err    error
	}
	calls []string
}

func (s *stubRunner) on(prefix, out string, err error) {
	s.rules = append([]struct {
		prefix string
		out    string
		err    error
	}{{prefix, out, err}}, s.rules...)
}

func (s *stubRunner) Output(_ context.Context, argv ...string) (string, error) {
	joined := strings.Join(argv, " ")
	s.calls = append(s.calls, joined)

	for _, r := range s.rules {
		if strings.HasPrefix(joined, r.prefix) {
			return r.out, r.err
		}
	}

	return "", nil
}

func TestListTierNamespacesSelectsByLabel(t *testing.T) {
	runner := &stubRunner{}
	runner.on("kubectl get namespaces", `{"items":[
		{"metadata":{"name":"emp-jdoe","labels":{"tenancy.truvity.io/tier":"employee"}}},
		{"metadata":{"name":"ci-truvity-bar","labels":{"tenancy.truvity.io/tier":"ci"}}}
	]}`, nil)

	client := engine.ExecClient{Runner: runner}

	namespaces, err := client.ListTierNamespaces(context.Background(),
		"tenancy.truvity.io/tier", []string{"ci", "employee"})
	require.NoError(t, err)

	assert.Equal(t, []engine.Namespace{
		{Name: "emp-jdoe", Tier: "employee"},
		{Name: "ci-truvity-bar", Tier: "ci"},
	}, namespaces)

	// The selection happens AT the API server, by label — gemaal-system
	// (no tier label) can never be returned.
	require.Len(t, runner.calls, 1)
	assert.Contains(t, runner.calls[0], `-l tenancy.truvity.io/tier in (ci,employee)`)
}

func TestListTierNamespacesNoTiersMeansNoReach(t *testing.T) {
	runner := &stubRunner{}
	client := engine.ExecClient{Runner: runner}

	namespaces, err := client.ListTierNamespaces(context.Background(), "tenancy.truvity.io/tier", nil)
	require.NoError(t, err)
	assert.Empty(t, namespaces)
	assert.Empty(t, runner.calls, "no configured tiers, no API call, no reach")
}

func TestListReleasesParsesHelmOutput(t *testing.T) {
	runner := &stubRunner{}
	runner.on("helm --kube-context devel@oidc list", `[
		{"name":"myapp","namespace":"emp-jdoe","updated":"2026-08-06 10:00:00.123456789 +0200 CEST","status":"deployed"},
		{"name":"myapp-infra","namespace":"emp-jdoe","updated":"2026-08-05T10:00:00Z","status":"deployed"}
	]`, nil)

	client := engine.ExecClient{Kubecontext: "devel@oidc", Runner: runner}

	releases, err := client.ListReleases(context.Background(), "emp-jdoe")
	require.NoError(t, err)
	require.Len(t, releases, 2)
	assert.Equal(t, "myapp", releases[0].Name)
	assert.Equal(t, "deployed", releases[0].Status)
	assert.Equal(t, time.Date(2026, 8, 6, 8, 0, 0, 123456789, time.UTC), releases[0].Updated.UTC())

	require.Len(t, runner.calls, 1)
	assert.Contains(t, runner.calls[0], "--namespace emp-jdoe --all --max 0 --output json")
}

func TestListReleasesRefusesUnparseableTimestamp(t *testing.T) {
	// An unparseable age must be an error, never "assume it is old": the
	// age is the only thing standing between a release and a TTL delete.
	runner := &stubRunner{}
	runner.on("helm list", `[{"name":"myapp","namespace":"emp-jdoe","updated":"yesterday","status":"deployed"}]`, nil)

	client := engine.ExecClient{Runner: runner}

	_, err := client.ListReleases(context.Background(), "emp-jdoe")
	require.ErrorContains(t, err, "unrecognized helm updated timestamp")
}

func TestReleaseSecretsKeepsNewestRevision(t *testing.T) {
	runner := &stubRunner{}
	runner.on("kubectl get secrets", `{"items":[
		{"metadata":{"name":"sh.helm.release.v1.myapp.v1","labels":{"owner":"helm","name":"myapp","version":"1","gemaal.io/ttl":"1h"}}},
		{"metadata":{"name":"sh.helm.release.v1.myapp.v12","labels":{"owner":"helm","name":"myapp","version":"12","gemaal.io/ttl":"48h"}}},
		{"metadata":{"name":"unrelated","labels":{"owner":"helm"}}}
	]}`, nil)

	client := engine.ExecClient{Runner: runner}

	secrets, err := client.ReleaseSecrets(context.Background(), "emp-jdoe")
	require.NoError(t, err)
	require.Len(t, secrets, 1)
	assert.Equal(t, 12, secrets[0].Version, "v12 beats v1 numerically, not lexically")
	assert.Equal(t, "48h", secrets[0].Labels["gemaal.io/ttl"])

	require.Len(t, runner.calls, 1)
	assert.Contains(t, runner.calls[0], "-l owner=helm")
}

func TestLabelSecretArgv(t *testing.T) {
	runner := &stubRunner{}
	client := engine.ExecClient{Runner: runner}

	err := client.LabelSecret(context.Background(), "emp-jdoe", "sh.helm.release.v1.myapp.v3",
		map[string]string{"gemaal.io/keep-until": "20260807T120000Z"})
	require.NoError(t, err)

	require.Len(t, runner.calls, 1)
	assert.Equal(t,
		"kubectl label secret sh.helm.release.v1.myapp.v3 --namespace emp-jdoe --overwrite gemaal.io/keep-until=20260807T120000Z",
		runner.calls[0])
}

func TestUninstallArgv(t *testing.T) {
	runner := &stubRunner{}
	client := engine.ExecClient{Kubecontext: "devel@oidc", Runner: runner}

	require.NoError(t, client.Uninstall(context.Background(), "ci-truvity-bar", "r1-a1"))

	require.Len(t, runner.calls, 1)
	assert.Equal(t,
		"helm --kube-context devel@oidc uninstall r1-a1 --namespace ci-truvity-bar --ignore-not-found --wait --timeout 10m",
		runner.calls[0])
}
