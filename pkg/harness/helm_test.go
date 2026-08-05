package harness

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallArgv(t *testing.T) {
	s := &stubRunner{}
	c := &Cluster{Kubecontext: "devel@oidc", Kubeconfig: "/tmp/kc", Runner: s}

	err := c.Install(context.Background(), "emp-jdoe", Install{
		Release:     "myapp",
		Chart:       "./dist/myapp-1.2.3.tgz",
		ValuesFiles: []string{"a.yaml", "b.yaml"},
		Set:         []string{"k=v"},
		Labels: Labels{
			TTL:         "24h",
			KeepUntil:   time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC),
			ExecutionID: "r12-a1",
		},
	})
	require.NoError(t, err)

	require.Len(t, s.calls, 1)
	assert.Equal(t, []string{
		"helm", "upgrade", "--install", "myapp", "./dist/myapp-1.2.3.tgz",
		"--namespace", "emp-jdoe",
		"--kube-context", "devel@oidc",
		"--kubeconfig", "/tmp/kc",
		"--wait", "--timeout", "5m0s",
		"--labels", "gemaal.io/ttl=24h,gemaal.io/keep-until=20260805T120000Z,gemaal.io/execution-id=r12-a1",
		"--values", "a.yaml",
		"--values", "b.yaml",
		"--set", "k=v",
	}, s.calls[0])
}

func TestInstallWithoutLabelsCarriesNoLabelsFlag(t *testing.T) {
	s := &stubRunner{}
	c := &Cluster{Runner: s}

	require.NoError(t, c.Install(context.Background(), "emp-jdoe", Install{Release: "myapp", Chart: "chart"}))
	require.Len(t, s.calls, 1)
	assert.NotContains(t, s.calls[0], "--labels")
	assert.NotContains(t, s.joined()[0], "--kube-context", "no kubecontext flag when none is set")
}

func TestInstallCustomLabelDomain(t *testing.T) {
	s := &stubRunner{}
	c := &Cluster{Runner: s, LabelDomain: "pump.example.com"}

	require.NoError(t, c.Install(context.Background(), "emp-jdoe",
		Install{Release: "myapp", Chart: "chart", Labels: Labels{TTL: "8h"}}))
	assert.Contains(t, s.calls[0], "pump.example.com/ttl=8h")
}

func TestInstallRefusals(t *testing.T) {
	tests := []struct {
		name    string
		install Install
		wantErr string
	}{
		{
			name:    "bad ttl",
			install: Install{Release: "myapp", Chart: "chart", Labels: Labels{TTL: "soon"}},
			wantErr: "ttl label",
		},
		{
			name:    "bad execution id",
			install: Install{Release: "myapp", Chart: "chart", Labels: Labels{ExecutionID: "no spaces allowed"}},
			wantErr: "not a valid label value",
		},
		{
			name:    "release over the bound",
			install: Install{Release: strings.Repeat("x", 54), Chart: "chart"},
			wantErr: "53-character bound",
		},
		{
			name:    "release shape",
			install: Install{Release: "MyApp", Chart: "chart"},
			wantErr: "RFC1123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &stubRunner{}
			c := &Cluster{Runner: s}

			err := c.Install(context.Background(), "emp-jdoe", tt.install)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Empty(t, s.calls, "refusals must happen before helm runs")
		})
	}
}

func TestInstallPairOrder(t *testing.T) {
	s := &stubRunner{}
	c := &Cluster{Runner: s}
	tenant := Tenant{Namespace: "emp-jdoe", Release: "url-shortener"}

	err := c.InstallPair(context.Background(), tenant, Pair{
		InfraChart:  "infra.tgz",
		AppChart:    "app.tgz",
		ValuesFiles: []string{"v.yaml"},
		Labels:      Labels{ExecutionID: "r12-a1"},
	})
	require.NoError(t, err)

	require.Len(t, s.calls, 2)
	assert.Contains(t, s.joined()[0], "--install url-shortener-infra infra.tgz",
		"ring2 installs FIRST — the app's pre-install hooks need the infrastructure standing")
	assert.Contains(t, s.joined()[1], "--install url-shortener app.tgz")

	// Both releases share values and the ledger stamp.
	for _, call := range s.joined() {
		assert.Contains(t, call, "--values v.yaml")
		assert.Contains(t, call, "gemaal.io/execution-id=r12-a1")
	}
}

func TestInstallPairStopsWhenInfraFails(t *testing.T) {
	s := &stubRunner{}
	s.on("helm upgrade --install url-shortener-infra", "", errors.New("boom"))

	c := &Cluster{Runner: s}

	err := c.InstallPair(context.Background(), Tenant{Namespace: "emp-jdoe", Release: "url-shortener"},
		Pair{InfraChart: "infra.tgz", AppChart: "app.tgz"})
	require.Error(t, err)
	assert.Len(t, s.calls, 1, "the app must not install on top of failed infrastructure")
}

func TestInstallPairAssertsTheInfraBound(t *testing.T) {
	s := &stubRunner{}
	c := &Cluster{Runner: s}

	// 48 characters is a legal release, but its "-infra" companion is 54.
	release := strings.Repeat("x", 48)

	err := c.InstallPair(context.Background(), Tenant{Namespace: "emp-jdoe", Release: release},
		Pair{InfraChart: "infra.tgz", AppChart: "app.tgz"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "53-character bound")
	assert.Empty(t, s.calls)
}

func TestUninstallArgv(t *testing.T) {
	s := &stubRunner{}
	c := &Cluster{Kubecontext: "devel@oidc", Runner: s, HelmTimeout: 10 * time.Minute}

	require.NoError(t, c.Uninstall(context.Background(), "emp-jdoe", "myapp"))
	require.Len(t, s.calls, 1)
	assert.Equal(t, []string{
		"helm", "uninstall", "myapp", "--namespace", "emp-jdoe",
		"--kube-context", "devel@oidc",
		"--ignore-not-found", "--wait", "--timeout", "10m0s",
	}, s.calls[0])
}

func TestUninstallPairReverseOrderAndResilience(t *testing.T) {
	s := &stubRunner{}
	s.on("helm uninstall url-shortener --namespace", "", errors.New("app refused"))

	c := &Cluster{Runner: s}

	err := c.UninstallPair(context.Background(), Tenant{Namespace: "emp-jdoe", Release: "url-shortener"})
	require.Error(t, err, "the app failure must surface")

	require.Len(t, s.calls, 2, "the infra uninstall still runs after the app one fails")
	assert.Contains(t, s.joined()[0], "uninstall url-shortener --namespace",
		"the app uninstalls FIRST so it never outlives its infrastructure")
	assert.Contains(t, s.joined()[1], "uninstall url-shortener-infra --namespace")
}

func TestClusterValuesFile(t *testing.T) {
	path, err := ClusterValuesFile(map[string]any{"region": "eu-central-1", "nested": map[string]any{"flag": true}})
	require.NoError(t, err)

	t.Cleanup(func() { _ = os.Remove(path) })

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	content := string(data)
	assert.Contains(t, content, "gemaal:", "cluster values ride as .Values.gemaal")
	assert.Contains(t, content, "region: eu-central-1")
	assert.Contains(t, content, "flag: true")
}

func TestDefaultExecutionID(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

	t.Run("CI uses the run coordinates", func(t *testing.T) {
		t.Setenv(EnvCIRunNumber, "77")
		t.Setenv(EnvCIRunAttempt, "2")

		assert.Equal(t, "r77-a2", DefaultExecutionID(now))
	})

	t.Run("local runs are timestamped", func(t *testing.T) {
		t.Setenv(EnvCIRunNumber, "")

		assert.Equal(t, "local-20260805T120000Z", DefaultExecutionID(now))
	})
}
