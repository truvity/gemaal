package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// EnvTestKindKubeconfig points this integration test at a REAL kind
// cluster's kubeconfig. It is the one harness test that talks to an
// actual cluster instead of a stubRunner — see
// TestServiceURLAgainstRealKindCluster for exactly what it does and
// cleans up.
const EnvTestKindKubeconfig = "GEMAAL_TEST_KIND_KUBECONFIG"

// TestServiceURLAgainstRealKindCluster proves the kind tier end to end
// against a real cluster: create a tiny Deployment + Service in a
// scratch namespace, reach it through (*Cluster).ServiceURL's
// port-forward, then clean up. SKIPPED unless EnvTestKindKubeconfig is
// set — this package must never create a kind cluster itself (a
// machine's kind cluster is a shared, singular resource; a second one
// breaks it), so a real cluster is a precondition the operator
// supplies, never something this test brings up.
//
// Run it against an existing kind cluster with:
//
//	GEMAAL_TEST_KIND_KUBECONFIG=$(kind get kubeconfig-path --name <cluster> 2>/dev/null || echo ~/.kube/config) \
//	    go test ./pkg/harness/ -run TestServiceURLAgainstRealKindCluster -v
//
// or, simpler, when the ambient kubeconfig already points at the kind
// cluster:
//
//	GEMAAL_TEST_KIND_KUBECONFIG=$KUBECONFIG go test ./pkg/harness/ -run TestServiceURLAgainstRealKindCluster -v
func TestServiceURLAgainstRealKindCluster(t *testing.T) {
	kubeconfig := os.Getenv(EnvTestKindKubeconfig)
	if kubeconfig == "" {
		t.Skipf("SKIPPED: set %s to a kind cluster's kubeconfig to run this test against it", EnvTestKindKubeconfig)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	runner := ExecRunner{}
	kubectlArgs := func(args ...string) []string {
		return append([]string{"kubectl", "--kubeconfig", kubeconfig}, args...)
	}

	namespace := fmt.Sprintf("gemaal-harness-it-%d", time.Now().UnixNano())

	require.NoError(t, runner.Run(ctx, kubectlArgs("create", "namespace", namespace)...),
		"a namespace-create failure here means the kubeconfig does not reach a live cluster")

	t.Cleanup(func() {
		// A fresh, un-canceled context: cleanup must run even when the
		// test's own ctx is already done.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()

		_ = runner.Run(cleanupCtx, kubectlArgs("delete", "namespace", namespace, "--ignore-not-found", "--wait=false")...)
	})

	manifestPath := filepath.Join(t.TempDir(), "echo.yaml")
	require.NoError(t, os.WriteFile(manifestPath, []byte(echoManifest(namespace)), 0o600))

	require.NoError(t, runner.Run(ctx, kubectlArgs("apply", "-n", namespace, "-f", manifestPath)...))
	require.NoError(t, runner.Run(ctx,
		kubectlArgs("rollout", "status", "deployment/echo", "-n", namespace, "--timeout=90s")...))

	cluster := &Cluster{Kubeconfig: kubeconfig, Tier: TierKind}
	t.Cleanup(cluster.CloseForwards)

	// The Service port (8080) deliberately differs from the container
	// port (80, named "http") — the exact shape that broke when the
	// forward dialed the SERVICE port instead of the endpoint's own
	// resolved port. A same-numbered port/targetPort pair would not
	// have caught that regression.
	url, err := cluster.ServiceURL(ctx, namespace, "echo", 8080)
	require.NoError(t, err, "ServiceURL must open a port-forward and return a reachable local URL")
	t.Logf("kind tier ServiceURL: %s", url)

	require.NoError(t, WaitHTTPReady(ctx, url, 30*time.Second),
		"the forwarded URL must actually reach the pod's HTTP server")
}

// echoManifest is a Deployment + Service small enough to schedule
// instantly on kind and to serve an HTTP response with no readiness
// probe needed (nginx:alpine listens the moment the container starts).
// The Service's port (8080) is deliberately NOT the container's port
// (80, reached by the NAME "http", not by number) — the exact shape
// that exposed the "wrong port when port != targetPort" defect: forwarding
// to the Service's own port number instead of the endpoint's resolved
// one would have connected to nothing.
func echoManifest(namespace string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: echo
  namespace: %[1]s
spec:
  replicas: 1
  selector:
    matchLabels: {app: echo}
  template:
    metadata:
      labels: {app: echo}
    spec:
      containers:
        - name: echo
          image: nginx:alpine
          ports:
            - name: http
              containerPort: 80
---
apiVersion: v1
kind: Service
metadata:
  name: echo
  namespace: %[1]s
spec:
  selector: {app: echo}
  ports:
    - port: 8080
      targetPort: http
`, namespace)
}
