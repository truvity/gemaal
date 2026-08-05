package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/gemaal/pkg/harness"
	"github.com/truvity/gemaal/pkg/identity"
)

// cliStubRunner records helm/kubectl argv instead of executing.
type cliStubRunner struct {
	calls [][]string
}

func (s *cliStubRunner) Run(_ context.Context, argv ...string) error {
	s.calls = append(s.calls, argv)

	return nil
}

func (s *cliStubRunner) Output(_ context.Context, argv ...string) (string, error) {
	s.calls = append(s.calls, argv)

	return "", nil
}

func (s *cliStubRunner) joined() []string {
	out := make([]string, len(s.calls))
	for i, c := range s.calls {
		out[i] = strings.Join(c, " ")
	}

	return out
}

// stubTheRunner swaps the CLI's executor for the test's lifetime.
func stubTheRunner(t *testing.T) *cliStubRunner {
	t.Helper()

	s := &cliStubRunner{}
	prev := newRunner
	newRunner = func() harness.Runner { return s }

	t.Cleanup(func() { newRunner = prev })

	return s
}

// clearTenantEnv neutralizes the resolution environment — including the
// CI variables a real runner sets.
func clearTenantEnv(t *testing.T) {
	t.Helper()

	for _, k := range []string{
		harness.EnvNamespace, harness.EnvRelease, harness.EnvKubecontext,
		harness.EnvCIRunNumber, harness.EnvCIRunAttempt, identity.EnvEmail,
	} {
		t.Setenv(k, "")
	}
}

// writeGemaalYAML writes a config with an identity map and cluster
// values, returning its path.
func writeGemaalYAML(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "gemaal.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
clusters:
  devel@oidc:
    values:
      region: eu-central-1
identity:
  emails:
    j.doe@example.com: jdoe
`), 0o600))

	return path
}

func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer

	cmd := newCommand()
	cmd.Writer = &out

	err := cmd.Run(context.Background(), append([]string{"gemaalctl"}, args...))

	return out.String(), err
}

func TestHelpListsTenantCommands(t *testing.T) {
	out, err := runCLI(t, "--help")
	require.NoError(t, err)

	for _, name := range []string{"whoami", "install", "uninstall"} {
		assert.Contains(t, out, name)
	}
}

func TestWhoamiResolvesTheFullChain(t *testing.T) {
	clearTenantEnv(t)
	t.Setenv(identity.EnvEmail, "j.doe@example.com")

	out, err := runCLI(t, "whoami", "--config", writeGemaalYAML(t), "--app", "myapp")
	require.NoError(t, err)

	assert.Contains(t, out, "evidence:  env: j.doe@example.com")
	assert.Contains(t, out, "email:     j.doe@example.com (env)")
	assert.Contains(t, out, "slug:      jdoe")
	assert.Contains(t, out, "namespace: emp-jdoe")
	assert.Contains(t, out, "release:   myapp")
}

func TestWhoamiPrintsHalvesIndependently(t *testing.T) {
	clearTenantEnv(t)
	t.Setenv(identity.EnvEmail, "nobody@example.com")

	// Mapped nowhere and no app: slug, namespace and release all fail —
	// and whoami still reports each one instead of dying on the first.
	out, err := runCLI(t, "whoami", "--config", writeGemaalYAML(t))
	require.NoError(t, err)

	assert.Contains(t, out, "email:     nobody@example.com (env)")
	assert.Contains(t, out, "slug:      (unmapped")
	assert.Contains(t, out, "namespace: (unresolved")
	assert.Contains(t, out, "release:   (unresolved")
}

func TestInstallStampsLabelsAndClusterValues(t *testing.T) {
	clearTenantEnv(t)

	s := stubTheRunner(t)

	out, err := runCLI(t, "install",
		"--config", writeGemaalYAML(t),
		"-c", "devel@oidc",
		"-n", "emp-jdoe", "-r", "myapp",
		"--chart", "app.tgz",
		"--ttl", "24h",
		"--execution-id", "x1",
		"-f", "extra.yaml",
	)
	require.NoError(t, err)
	assert.Contains(t, out, "installed emp-jdoe/myapp")

	require.Len(t, s.calls, 1)
	call := s.joined()[0]
	assert.Contains(t, call, "helm upgrade --install myapp app.tgz --namespace emp-jdoe")
	assert.Contains(t, call, "--kube-context devel@oidc")
	assert.Contains(t, call, "--labels gemaal.io/ttl=24h,gemaal.io/execution-id=x1")

	// The cluster values ride first so the caller's -f can override them.
	values := valuesArgs(s.calls[0])
	require.Len(t, values, 2)
	assert.Contains(t, values[0], "gemaal-values-")
	assert.Equal(t, "extra.yaml", values[1])
}

func TestInstallDerivesAnExecutionID(t *testing.T) {
	clearTenantEnv(t)

	s := stubTheRunner(t)

	_, err := runCLI(t, "install", "-n", "emp-jdoe", "-r", "myapp", "--chart", "app.tgz")
	require.NoError(t, err)

	require.Len(t, s.calls, 1)
	assert.Contains(t, s.joined()[0], "gemaal.io/execution-id=local-",
		"an unnamed execution still gets a distinct id — re-run filtering depends on it")
}

func TestInstallPairInstallsInfraFirst(t *testing.T) {
	clearTenantEnv(t)

	s := stubTheRunner(t)

	_, err := runCLI(t, "install",
		"-n", "emp-jdoe", "-r", "myapp",
		"--chart", "app.tgz", "--infra-chart", "infra.tgz",
	)
	require.NoError(t, err)

	require.Len(t, s.calls, 2)
	assert.Contains(t, s.joined()[0], "--install myapp-infra infra.tgz")
	assert.Contains(t, s.joined()[1], "--install myapp app.tgz")
}

func TestUninstallRemovesThePairInReverse(t *testing.T) {
	clearTenantEnv(t)

	s := stubTheRunner(t)

	out, err := runCLI(t, "uninstall", "-n", "emp-jdoe", "-r", "myapp")
	require.NoError(t, err)
	assert.Contains(t, out, "uninstalled emp-jdoe/myapp")

	require.Len(t, s.calls, 2)
	assert.Contains(t, s.joined()[0], "helm uninstall myapp --namespace emp-jdoe")
	assert.Contains(t, s.joined()[1], "helm uninstall myapp-infra --namespace emp-jdoe")

	for _, call := range s.joined() {
		assert.Contains(t, call, "--ignore-not-found")
	}
}

func TestInstallRequiresAChart(t *testing.T) {
	clearTenantEnv(t)

	_, err := runCLI(t, "install", "-n", "emp-jdoe", "-r", "myapp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chart")
}

func TestInstallBadKeepUntil(t *testing.T) {
	clearTenantEnv(t)

	stubTheRunner(t)

	_, err := runCLI(t, "install", "-n", "emp-jdoe", "-r", "myapp", "--chart", "app.tgz", "--keep-until", "tomorrow")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--keep-until")
}

// valuesArgs extracts every --values argument from an argv.
func valuesArgs(argv []string) []string {
	var out []string

	for i, a := range argv {
		if a == "--values" && i+1 < len(argv) {
			out = append(out, argv[i+1])
		}
	}

	return out
}
