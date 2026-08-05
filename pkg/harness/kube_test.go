package harness

import (
	"context"
	"errors"
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
