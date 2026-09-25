package harness

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeForwardProcess is forwardProcess scripted for tests — the
// port-forward equivalent of stubRunner: an ordered line queue, an
// optional terminal error, and a close flag tests assert on. A queue
// that runs out with no terminal error blocks (like a real still-running
// process would) until the test's own timeout fires.
type fakeForwardProcess struct {
	mu     sync.Mutex
	lines  []string
	err    error
	closed bool
}

func (f *fakeForwardProcess) Line() (string, error) {
	f.mu.Lock()
	if len(f.lines) > 0 {
		l := f.lines[0]
		f.lines = f.lines[1:]
		f.mu.Unlock()

		return l, nil
	}

	err := f.err
	f.mu.Unlock()

	if err != nil {
		return "", err
	}

	select {} // still running: never returns on its own
}

func (f *fakeForwardProcess) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true

	return nil
}

func (f *fakeForwardProcess) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.closed
}

func TestParseForwardingLine(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantPort string
		wantOK   bool
	}{
		{"the real announcement", "Forwarding from 127.0.0.1:54321 -> 8080", "54321", true},
		{"leading/trailing whitespace is trimmed", "  Forwarding from 127.0.0.1:1234 -> 80  ", "1234", true},
		{"the IPv6 twin kubectl also prints is not matched", "Forwarding from [::1]:54321 -> 8080", "", false},
		{"a per-request noise line is not the readiness line", "Handling connection for 54321", "", false},
		{"empty line", "", "", false},
		{"unrelated stderr-shaped text", "error: unable to forward port", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port, ok := parseForwardingLine(tt.line)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantPort, port)
		})
	}
}

func TestWaitForForwardReady(t *testing.T) {
	t.Run("returns the port once the Forwarding line appears", func(t *testing.T) {
		proc := &fakeForwardProcess{lines: []string{"Forwarding from 127.0.0.1:54321 -> 8080"}}

		port, err := waitForForwardReady(proc, time.Second)
		require.NoError(t, err)
		assert.Equal(t, "54321", port)
	})

	t.Run("skips noise lines before the readiness line", func(t *testing.T) {
		proc := &fakeForwardProcess{lines: []string{
			"Handling connection for 8080",
			"Forwarding from 127.0.0.1:9999 -> 8080",
		}}

		port, err := waitForForwardReady(proc, time.Second)
		require.NoError(t, err)
		assert.Equal(t, "9999", port)
	})

	t.Run("a process error surfaces before any readiness line", func(t *testing.T) {
		proc := &fakeForwardProcess{err: errors.New("kubectl: pod not found")}

		_, err := waitForForwardReady(proc, time.Second)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "pod not found")
	})

	t.Run("a still-running process with no readiness line times out", func(t *testing.T) {
		proc := &fakeForwardProcess{}

		_, err := waitForForwardReady(proc, 20*time.Millisecond)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out")
	})
}

func TestServiceURLKindTier(t *testing.T) {
	t.Run("opens a forward straight to the endpoint pod and returns the local URL", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl --context kind-mycluster get endpoints myapp-web -n ci-kind-suite", "myapp-web-7d4f9-abcde\n", nil)

		fake := &fakeForwardProcess{lines: []string{"Forwarding from 127.0.0.1:54321 -> 8080"}}

		var startedArgv []string

		c := &Cluster{
			Kubecontext: "kind-mycluster",
			Runner:      s,
			forwardStart: func(_ context.Context, argv []string) (forwardProcess, error) {
				startedArgv = argv

				return fake, nil
			},
		}

		url, err := c.ServiceURL(context.Background(), "ci-kind-suite", "myapp-web", 8080)
		require.NoError(t, err)
		assert.Equal(t, "http://127.0.0.1:54321", url)

		assert.Equal(t, []string{
			"kubectl", "--context", "kind-mycluster",
			"port-forward", "-n", "ci-kind-suite", "pod/myapp-web-7d4f9-abcde", ":8080",
		}, startedArgv)

		fw, ok := c.ForwardFor("ci-kind-suite", "myapp-web")
		require.True(t, ok)
		assert.Equal(t, "myapp-web-7d4f9-abcde", fw.Pod)
		assert.Equal(t, "127.0.0.1:54321", fw.LocalAddr)

		c.CloseForwards()
		assert.True(t, fake.isClosed(), "CloseForwards must stop the process")

		_, ok = c.ForwardFor("ci-kind-suite", "myapp-web")
		assert.False(t, ok, "a closed forward is forgotten")
	})

	t.Run("a service with no ready endpoint refuses before starting a forward", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get endpoints", "", nil)

		c := &Cluster{Tier: TierKind, Runner: s, forwardStart: func(context.Context, []string) (forwardProcess, error) {
			t.Fatal("must not start a forward with no resolved pod")

			return nil, nil
		}}

		_, err := c.ServiceURL(context.Background(), "ci-kind-suite", "myapp-web", 8080)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no ready endpoint")
	})

	t.Run("a forward that never becomes ready is closed and reported", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get endpoints", "myapp-web-abcde\n", nil)

		fake := &fakeForwardProcess{} // never emits the Forwarding line

		c := &Cluster{Tier: TierKind, Runner: s, ForwardReadyTimeout: 10 * time.Millisecond,
			forwardStart: func(context.Context, []string) (forwardProcess, error) { return fake, nil }}

		_, err := c.ServiceURL(context.Background(), "ci-kind-suite", "myapp-web", 8080)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "myapp-web-abcde", "the error names the pod it tried to reach")
		assert.True(t, fake.isClosed(), "a failed readiness wait must still close the process")
	})

	t.Run("explicit Tier field takes the kind path exactly like a kind- context", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl --context devel@oidc get endpoints", "pod-x\n", nil)

		fake := &fakeForwardProcess{lines: []string{"Forwarding from 127.0.0.1:1111 -> 80"}}

		c := &Cluster{Tier: TierKind, Kubecontext: "devel@oidc", Runner: s,
			forwardStart: func(context.Context, []string) (forwardProcess, error) { return fake, nil }}

		url, err := c.ServiceURL(context.Background(), "ns", "svc", 80)
		require.NoError(t, err)
		assert.Equal(t, "http://127.0.0.1:1111", url)
	})
}

func TestServiceURLSharedTierUnaffectedByKindMachinery(t *testing.T) {
	// The regression this guards: adding the kind path must not change
	// a single byte of the shared tier's existing behavior. No Tier, no
	// kind- context: ServiceURL must take the OLD direct-ClusterIP path
	// and never call get endpoints or a forwardStart.
	s := &stubRunner{}
	s.on("kubectl get svc", "10.96.0.5", nil)

	forwardStartCalled := false
	c := &Cluster{Runner: s, forwardStart: func(context.Context, []string) (forwardProcess, error) {
		forwardStartCalled = true

		return nil, errors.New("must not be reached on the shared tier")
	}}

	url, err := c.ServiceURL(context.Background(), "emp-jdoe", "myapp-web", 8080)
	require.NoError(t, err)
	assert.Equal(t, "http://10.96.0.5:8080", url)
	assert.False(t, forwardStartCalled)

	for _, call := range s.joined() {
		assert.NotContains(t, call, "get endpoints", "the shared tier resolves a ClusterIP, never a forward pod")
	}
}

func TestCloseForwardsIsANoOpWithNothingOpen(t *testing.T) {
	c := &Cluster{}
	assert.NotPanics(t, c.CloseForwards)
	c.CloseForwards() // and a second call
}

func TestPortForwardErrNamesTheForward(t *testing.T) {
	fw := &PortForward{Namespace: "ci-kind-suite", Service: "myapp-web", Pod: "myapp-web-abcde", LocalAddr: "127.0.0.1:54321"}

	err := fw.Err(errors.New("connection refused"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "myapp-web-abcde")
	assert.Contains(t, err.Error(), "ci-kind-suite/myapp-web")
	assert.Contains(t, err.Error(), "connection refused")
}
