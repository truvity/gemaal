package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServiceClusterIP(t *testing.T) {
	t.Run("resolves and trims", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl --context devel@oidc get svc myapp-web", "10.96.0.5\n", nil)

		c := &Cluster{Kubecontext: "devel@oidc", Runner: s}

		ip, err := c.ServiceClusterIP(context.Background(), "emp-jdoe", "myapp-web")
		require.NoError(t, err)
		assert.Equal(t, "10.96.0.5", ip)

		require.Len(t, s.calls, 1)
		assert.Equal(t, []string{
			"kubectl", "--context", "devel@oidc",
			"get", "svc", "myapp-web", "-n", "emp-jdoe",
			"-o", "jsonpath={.spec.clusterIP}",
		}, s.calls[0])
	})

	t.Run("headless service refuses", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get svc", "None", nil)

		_, err := (&Cluster{Runner: s}).ServiceClusterIP(context.Background(), "emp-jdoe", "db")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no ClusterIP")
	})

	t.Run("empty answer refuses", func(t *testing.T) {
		s := &stubRunner{}

		_, err := (&Cluster{Runner: s}).ServiceClusterIP(context.Background(), "emp-jdoe", "db")
		require.Error(t, err)
	})

	t.Run("kubectl failure is wrapped", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get svc", "", errors.New("not found"))

		_, err := (&Cluster{Runner: s}).ServiceClusterIP(context.Background(), "emp-jdoe", "db")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "emp-jdoe/db")
	})
}

func TestServiceURL(t *testing.T) {
	s := &stubRunner{}
	s.on("kubectl get svc", "10.96.0.5", nil)

	url, err := (&Cluster{Runner: s}).ServiceURL(context.Background(), "emp-jdoe", "myapp-web", 8080)
	require.NoError(t, err)
	assert.Equal(t, "http://10.96.0.5:8080", url)
}

const deploymentsJSON = `{
  "items": [
    {"metadata": {"name": "app-web", "annotations": {"meta.helm.sh/release-name": "url-shortener"}}},
    {"metadata": {"name": "app-redirect", "annotations": {"meta.helm.sh/release-name": "url-shortener"}}},
    {"metadata": {"name": "infra-nats", "annotations": {"meta.helm.sh/release-name": "url-shortener-infra"}}},
    {"metadata": {"name": "unrelated", "annotations": {"meta.helm.sh/release-name": "other"}}},
    {"metadata": {"name": "bare", "annotations": {}}}
  ]
}`

func TestReleaseDeployments(t *testing.T) {
	s := &stubRunner{}
	s.on("kubectl get deployments", deploymentsJSON, nil)

	c := &Cluster{Runner: s}

	names, err := c.ReleaseDeployments(context.Background(), "emp-jdoe", "url-shortener", "url-shortener-infra")
	require.NoError(t, err)
	assert.Equal(t, []string{"app-web", "app-redirect", "infra-nats"}, names)
}

func TestReleaseDeploymentsFallsBackToInstanceLabel(t *testing.T) {
	t.Run("annotation only (helm install/upgrade)", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get deployments", `{"items": [
			{"metadata": {"name": "app-web", "annotations": {"meta.helm.sh/release-name": "url-shortener"}}}
		]}`, nil)

		names, err := (&Cluster{Runner: s}).ReleaseDeployments(context.Background(), "emp-jdoe", "url-shortener")
		require.NoError(t, err)
		assert.Equal(t, []string{"app-web"}, names)
	})

	t.Run("label only (rendered by a GitOps controller, e.g. helm template)", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get deployments", `{"items": [
			{"metadata": {"name": "app-web", "labels": {"app.kubernetes.io/instance": "url-shortener"}}}
		]}`, nil)

		names, err := (&Cluster{Runner: s}).ReleaseDeployments(context.Background(), "emp-jdoe", "url-shortener")
		require.NoError(t, err)
		assert.Equal(t, []string{"app-web"}, names)
	})

	t.Run("annotation and label agree", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get deployments", `{"items": [
			{"metadata": {"name": "app-web",
				"annotations": {"meta.helm.sh/release-name": "url-shortener"},
				"labels": {"app.kubernetes.io/instance": "url-shortener"}}}
		]}`, nil)

		names, err := (&Cluster{Runner: s}).ReleaseDeployments(context.Background(), "emp-jdoe", "url-shortener")
		require.NoError(t, err)
		assert.Equal(t, []string{"app-web"}, names)
	})

	t.Run("annotation names release A, label names release B: belongs to A only", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get deployments", `{"items": [
			{"metadata": {"name": "app-web",
				"annotations": {"meta.helm.sh/release-name": "release-a"},
				"labels": {"app.kubernetes.io/instance": "release-b"}}}
		]}`, nil)

		c := &Cluster{Runner: s}

		names, err := c.ReleaseDeployments(context.Background(), "emp-jdoe", "release-a")
		require.NoError(t, err)
		assert.Equal(t, []string{"app-web"}, names, "annotation is authoritative when present")

		names, err = c.ReleaseDeployments(context.Background(), "emp-jdoe", "release-b")
		require.NoError(t, err)
		assert.Empty(t, names, "the label never overrides a present annotation")
	})

	t.Run("neither annotation nor label is excluded", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get deployments", `{"items": [
			{"metadata": {"name": "bare"}}
		]}`, nil)

		names, err := (&Cluster{Runner: s}).ReleaseDeployments(context.Background(), "emp-jdoe", "url-shortener")
		require.NoError(t, err)
		assert.Empty(t, names)
	})
}

func TestWaitForDeployments(t *testing.T) {
	t.Run("waits for each owned deployment", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get deployments", deploymentsJSON, nil)

		c := &Cluster{Runner: s, RolloutTimeout: 90 * time.Second}

		err := c.WaitForDeployments(context.Background(), "emp-jdoe", "url-shortener", "url-shortener-infra")
		require.NoError(t, err)

		joined := s.joined()
		require.Len(t, joined, 4, "one list + three rollout waits")
		assert.Contains(t, joined[1], "rollout status deployment/app-web -n emp-jdoe --timeout 1m30s")
		assert.Contains(t, joined[2], "rollout status deployment/app-redirect")
		assert.Contains(t, joined[3], "rollout status deployment/infra-nats")
	})

	t.Run("zero owned deployments is an error", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get deployments", `{"items": []}`, nil)

		err := (&Cluster{Runner: s}).WaitForDeployments(context.Background(), "emp-jdoe", "url-shortener")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no Deployments")
	})

	t.Run("a stuck rollout names the deployment", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get deployments", deploymentsJSON, nil)
		s.on("kubectl rollout status deployment/app-web", "", errors.New("timed out"))

		err := (&Cluster{Runner: s}).WaitForDeployments(context.Background(), "emp-jdoe", "url-shortener")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "emp-jdoe/app-web")
	})
}

const versionLabel = "app.kubernetes.io/version"

// deploymentJSON renders one Deployment's `kubectl get -o json` at a
// given generation/version, replica count and rollout progress — the
// exact shape WaitForDeploymentsAtVersion polls.
func deploymentJSON(generation int, version string, replicas, statusReplicas, updated, available int) string {
	return fmt.Sprintf(`{
		"metadata": {"generation": %d},
		"spec": {"replicas": %d, "template": {"metadata": {"labels": {"app.kubernetes.io/version": %q}}}},
		"status": {"observedGeneration": %d, "replicas": %d, "updatedReplicas": %d, "availableReplicas": %d}
	}`, generation, replicas, version, generation, statusReplicas, updated, available)
}

func TestWaitForDeploymentsAtVersion(t *testing.T) {
	t.Run("in-progress then complete", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get deployments", `{"items": [
			{"metadata": {"name": "app-stat", "annotations": {"meta.helm.sh/release-name": "url-shortener"}}}
		]}`, nil)
		// Poll 1: the pod template already names the new version (the
		// GitOps controller pushed it), but the rollout has not finished —
		// one old pod is still around and one new one is not yet
		// available. Poll 2: fully rolled out.
		s.onSeq("kubectl get deployment app-stat", []string{
			deploymentJSON(2, "v2", 2, 3, 1, 1),
			deploymentJSON(2, "v2", 2, 2, 2, 2),
		}, nil)

		c := &Cluster{Runner: s, RolloutTimeout: time.Second, RolloutPollInterval: time.Millisecond}

		err := c.WaitForDeploymentsAtVersion(context.Background(), "emp-jdoe", versionLabel, "v2", "url-shortener")
		require.NoError(t, err)

		joined := s.joined()
		require.GreaterOrEqual(t, len(joined), 3, "one list + at least two polls")
		assert.Contains(t, joined[1], "get deployment app-stat -n emp-jdoe -o json")
	})

	t.Run("stuck on the OLD version times out naming the deployment", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get deployments", `{"items": [
			{"metadata": {"name": "app-stat", "annotations": {"meta.helm.sh/release-name": "url-shortener"}}}
		]}`, nil)
		// The Deployment's own spec never moves to v2 within this
		// window — e.g. a GitOps controller has not yet applied the
		// promoted app release when this polls.
		s.on("kubectl get deployment app-stat", deploymentJSON(1, "v1", 2, 2, 2, 2), nil)

		c := &Cluster{Runner: s, RolloutTimeout: 20 * time.Millisecond, RolloutPollInterval: time.Millisecond}

		err := c.WaitForDeploymentsAtVersion(context.Background(), "emp-jdoe", versionLabel, "v2", "url-shortener")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "emp-jdoe/app-stat")
		assert.Contains(t, err.Error(), `want "v2"`)
	})

	t.Run("stuck mid-rollout at the right version times out naming what is still unready", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get deployments", `{"items": [
			{"metadata": {"name": "app-stat", "annotations": {"meta.helm.sh/release-name": "url-shortener"}}}
		]}`, nil)
		// The template is already v2, but one replica never becomes
		// available within this window (e.g. a CrashLoopBackOff).
		s.on("kubectl get deployment app-stat", deploymentJSON(2, "v2", 2, 2, 2, 1), nil)

		c := &Cluster{Runner: s, RolloutTimeout: 20 * time.Millisecond, RolloutPollInterval: time.Millisecond}

		err := c.WaitForDeploymentsAtVersion(context.Background(), "emp-jdoe", versionLabel, "v2", "url-shortener")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "emp-jdoe/app-stat")
		assert.Contains(t, err.Error(), "1/2 replicas available")
	})

	t.Run("zero owned deployments is an error", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get deployments", `{"items": []}`, nil)

		err := (&Cluster{Runner: s}).WaitForDeploymentsAtVersion(context.Background(), "emp-jdoe", versionLabel, "v2", "url-shortener")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no Deployments")
	})
}

func TestWaitHTTPReady(t *testing.T) {
	t.Run("any response is ready", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound) // a 404 still proves something is listening
		}))
		t.Cleanup(server.Close)

		require.NoError(t, WaitHTTPReady(context.Background(), server.URL, time.Second))
	})

	t.Run("nothing listening runs out of patience", func(t *testing.T) {
		err := WaitHTTPReady(context.Background(), "http://127.0.0.1:1", 10*time.Millisecond)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HTTP not ready")
	})
}
