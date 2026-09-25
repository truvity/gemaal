package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeForwardProcess is forwardProcess scripted for tests — the
// port-forward equivalent of stubRunner. Ready returns the configured
// result immediately when one is set, or blocks until ctx is done
// (simulating a still-starting or wedged process) when none is.
type fakeForwardProcess struct {
	port      string
	err       error
	hasResult bool

	mu     sync.Mutex
	closed bool
}

func (f *fakeForwardProcess) Ready(ctx context.Context) (string, error) {
	if f.hasResult {
		return f.port, f.err
	}

	<-ctx.Done()

	return "", ctx.Err()
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

// TestDrainForwardOutputUnderConnectionSpam is the regression for the
// stall defect: kubectl prints one "Handling connection for …" line per
// request, from inside the connection handler, for the WHOLE life of
// the forward. A drain that stops reading after readiness lets that
// stream fill the pipe (~64 KiB) and wedge kubectl — which reads as a
// service hang mid-suite after a couple of thousand requests. Feeding
// tens of thousands of such lines through a real io.Pipe with no
// synchronization beyond the pipe itself proves the writer never blocks
// (bounded time) and the reader goroutine exits (no leak) once the pipe
// closes.
func TestDrainForwardOutputUnderConnectionSpam(t *testing.T) {
	pr, pw := io.Pipe()

	ready := make(chan forwardReady, 1)

	var waited int
	wait := func() error { waited++; return nil }

	drainDone := make(chan struct{})

	go func() {
		defer close(drainDone)
		drainForwardOutput(pr, ready, wait)
	}()

	const spamLines = 50000

	writeDone := make(chan struct{})

	go func() {
		defer close(writeDone)

		_, _ = fmt.Fprintln(pw, "Forwarding from 127.0.0.1:54321 -> 8080")

		for i := 0; i < spamLines; i++ {
			_, _ = fmt.Fprintln(pw, "Handling connection for 8080")
		}

		_ = pw.Close()
	}()

	select {
	case r := <-ready:
		require.NoError(t, r.err)
		assert.Equal(t, "54321", r.port)
	case <-time.After(5 * time.Second):
		t.Fatal("readiness was never delivered")
	}

	select {
	case <-writeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the producer blocked — the drain stalled under connection-line volume after readiness")
	}

	select {
	case <-drainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain goroutine leaked after the pipe closed")
	}

	assert.Equal(t, 1, waited, "wait must be called exactly once, after every read completed")
}

func TestDrainForwardOutputProcessEndsBeforeReadiness(t *testing.T) {
	pr, pw := io.Pipe()
	ready := make(chan forwardReady, 1)
	wait := func() error { return errors.New("exit status 1") }

	done := make(chan struct{})

	go func() {
		defer close(done)
		drainForwardOutput(pr, ready, wait)
	}()

	_, _ = fmt.Fprintln(pw, "error: unable to forward port because pod is not running")
	_ = pw.Close()

	select {
	case r := <-ready:
		require.Error(t, r.err)
		assert.Contains(t, r.err.Error(), "before announcing readiness")
	case <-time.After(2 * time.Second):
		t.Fatal("no result delivered")
	}

	<-done
}

// TestExecForwardStartRealSubprocessDrainsAndClosesCleanly exercises the
// REAL execForwardStart — real exec.CommandContext, StdoutPipe, Kill and
// Wait — against a harmless bash subprocess standing in for kubectl (no
// cluster, no kubectl needed): it prints the Forwarding line, then a
// connection-spam volume well past a 64 KiB pipe's capacity, then
// sleeps. Under the old buffered-channel drain this reliably deadlocks
// (the scanner goroutine blocks on the full channel, the pipe fills,
// bash blocks writing, cmd.Wait() is never reached) — so a Close that
// returns promptly here is the real proof the fix works, not just the
// io.Pipe-level unit tests above.
func TestExecForwardStartRealSubprocessDrainsAndClosesCleanly(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}

	script := `echo "Forwarding from 127.0.0.1:19999 -> 80"
for i in $(seq 1 20000); do echo "Handling connection for 80"; done
sleep 30`

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	proc, err := execForwardStart(ctx, []string{"bash", "-c", script})
	require.NoError(t, err)

	readyCtx, readyCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readyCancel()

	port, err := proc.Ready(readyCtx)
	require.NoError(t, err)
	assert.Equal(t, "19999", port)

	// Give the spam loop a moment to actually run past the pipe's
	// buffer size; Close below is the real proof either way.
	time.Sleep(200 * time.Millisecond)

	closeDone := make(chan struct{})

	go func() {
		_ = proc.Close()
		close(closeDone)
	}()

	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return — the drain goroutine likely stalled on a filled pipe")
	}
}

func TestServiceURLKindTierDoesNotDieWithTheCallersContext(t *testing.T) {
	// The defect this guards: exec.CommandContext(ctx, …) tied the
	// subprocess to ServiceURL's own ctx. On the shared tier the
	// returned URL is just an IP and outlives a cancel(); the kind
	// tier's forward must do the same — the call site is supposed to be
	// IDENTICAL across tiers.
	s := &stubRunner{}
	s.on("kubectl get endpoints", `{"subsets":[{"addresses":[{"targetRef":{"name":"pod-x"}}],"ports":[{"port":8080}]}]}`, nil)

	fake := &fakeForwardProcess{port: "54321", hasResult: true}

	var startedCtx context.Context

	c := &Cluster{Tier: TierKind, Runner: s, forwardStart: func(ctx context.Context, _ []string) (forwardProcess, error) {
		startedCtx = ctx

		return fake, nil
	}}

	callerCtx, cancel := context.WithCancel(context.Background())

	url, err := c.ServiceURL(callerCtx, "ns", "svc", 8080)
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:54321", url)

	cancel() // the caller is done with its own ctx right after the call returns

	require.NotNil(t, startedCtx)
	assert.NoError(t, startedCtx.Err(), "the forward's own context must not be cancelled by the caller's ctx")
}

func TestServiceURLKindTierReadinessRespectsCallerCancellation(t *testing.T) {
	// The other half of the same fix: ctx must still bound the
	// READINESS wait, even though the process itself runs detached.
	s := &stubRunner{}
	s.on("kubectl get endpoints", `{"subsets":[{"addresses":[{"targetRef":{"name":"pod-x"}}],"ports":[{"port":8080}]}]}`, nil)

	fake := &fakeForwardProcess{} // never ready — Ready blocks on ctx.Done()

	c := &Cluster{Tier: TierKind, Runner: s, ForwardReadyTimeout: time.Minute,
		forwardStart: func(context.Context, []string) (forwardProcess, error) { return fake, nil }}

	callerCtx, cancel := context.WithCancel(context.Background())
	cancel() // already done before the call

	_, err := c.ServiceURL(callerCtx, "ns", "svc", 8080)
	require.Error(t, err, "a caller whose ctx is already done must not wait the full readiness timeout")
	assert.True(t, fake.isClosed())
}

func TestResolveForwardTarget(t *testing.T) {
	t.Run("single unnamed port: no service lookup needed", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get endpoints myapp-web -n ns",
			`{"subsets":[{"addresses":[{"targetRef":{"name":"myapp-web-abcde"}}],"ports":[{"port":9090}]}]}`, nil)

		c := &Cluster{Runner: s}

		target, err := c.resolveForwardTarget(context.Background(), "ns", "myapp-web", 80)
		require.NoError(t, err)
		assert.Equal(t, "myapp-web-abcde", target.pod)
		assert.Equal(t, 9090, target.port, "the SERVICE port (80) must never be used when targetPort differs")

		for _, call := range s.joined() {
			assert.NotContains(t, call, "get svc", "a single-port endpoint needs no name correlation")
		}
	})

	t.Run("single port with a named targetPort resolves to the endpoint's own number", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get endpoints",
			`{"subsets":[{"addresses":[{"targetRef":{"name":"echo-abc"}}],"ports":[{"name":"http","port":8080}]}]}`, nil)

		c := &Cluster{Runner: s}

		target, err := c.resolveForwardTarget(context.Background(), "ns", "echo", 8080)
		require.NoError(t, err)
		assert.Equal(t, 8080, target.port)
	})

	t.Run("multi-port service correlates by port NAME", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get endpoints myapp -n ns",
			`{"subsets":[{"addresses":[{"targetRef":{"name":"myapp-abc"}}],`+
				`"ports":[{"name":"web","port":8080},{"name":"tls","port":8443}]}]}`, nil)
		s.on("kubectl get svc myapp -n ns",
			`{"spec":{"ports":[{"name":"web","port":80},{"name":"tls","port":443}]}}`, nil)

		c := &Cluster{Runner: s}

		target, err := c.resolveForwardTarget(context.Background(), "ns", "myapp", 443)
		require.NoError(t, err)
		assert.Equal(t, "myapp-abc", target.pod)
		assert.Equal(t, 8443, target.port, "port 443 (name tls) must resolve to the tls endpoint port, not web's")
	})

	t.Run("multi-port service: the other port also resolves correctly", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get endpoints",
			`{"subsets":[{"addresses":[{"targetRef":{"name":"myapp-abc"}}],`+
				`"ports":[{"name":"web","port":8080},{"name":"tls","port":8443}]}]}`, nil)
		s.on("kubectl get svc",
			`{"spec":{"ports":[{"name":"web","port":80},{"name":"tls","port":443}]}}`, nil)

		c := &Cluster{Runner: s}

		target, err := c.resolveForwardTarget(context.Background(), "ns", "myapp", 80)
		require.NoError(t, err)
		assert.Equal(t, 8080, target.port)
	})

	t.Run("no ready endpoint refuses", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get endpoints", `{"subsets":[]}`, nil)

		c := &Cluster{Runner: s}

		_, err := c.resolveForwardTarget(context.Background(), "ns", "myapp", 80)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no ready endpoint")
	})

	t.Run("a service port absent from the spec refuses by name", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get endpoints",
			`{"subsets":[{"addresses":[{"targetRef":{"name":"myapp-abc"}}],`+
				`"ports":[{"name":"web","port":8080},{"name":"tls","port":8443}]}]}`, nil)
		s.on("kubectl get svc", `{"spec":{"ports":[{"name":"web","port":80},{"name":"tls","port":443}]}}`, nil)

		c := &Cluster{Runner: s}

		_, err := c.resolveForwardTarget(context.Background(), "ns", "myapp", 9999)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no port 9999")
	})
}

func TestServiceURLKindTier(t *testing.T) {
	t.Run("opens a forward straight to the endpoint pod and returns the local URL", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl --context kind-mycluster get endpoints myapp-web -n ci-kind-suite",
			`{"subsets":[{"addresses":[{"targetRef":{"name":"myapp-web-7d4f9-abcde"}}],"ports":[{"port":8080}]}]}`, nil)

		fake := &fakeForwardProcess{port: "54321", hasResult: true}

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

	t.Run("the remote side of the forward is the RESOLVED container port, not the Service port", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get endpoints",
			`{"subsets":[{"addresses":[{"targetRef":{"name":"echo-abc"}}],"ports":[{"name":"http","port":9090}]}]}`, nil)

		fake := &fakeForwardProcess{port: "1", hasResult: true}

		var startedArgv []string

		c := &Cluster{Tier: TierKind, Runner: s, forwardStart: func(_ context.Context, argv []string) (forwardProcess, error) {
			startedArgv = argv

			return fake, nil
		}}

		_, err := c.ServiceURL(context.Background(), "ns", "echo", 8080)
		require.NoError(t, err)
		assert.Contains(t, startedArgv, ":9090", "8080 is the Service port; 9090 is what the endpoint actually listens on")
		assert.NotContains(t, startedArgv, ":8080")
	})

	t.Run("a service with no ready endpoint refuses before starting a forward", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl get endpoints", `{"subsets":[]}`, nil)

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
		s.on("kubectl get endpoints",
			`{"subsets":[{"addresses":[{"targetRef":{"name":"myapp-web-abcde"}}],"ports":[{"port":8080}]}]}`, nil)

		fake := &fakeForwardProcess{} // never ready

		c := &Cluster{Tier: TierKind, Runner: s, ForwardReadyTimeout: 10 * time.Millisecond,
			forwardStart: func(context.Context, []string) (forwardProcess, error) { return fake, nil }}

		_, err := c.ServiceURL(context.Background(), "ci-kind-suite", "myapp-web", 8080)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "myapp-web-abcde", "the error names the pod it tried to reach")
		assert.True(t, fake.isClosed(), "a failed readiness wait must still close the process")
	})

	t.Run("explicit Tier field takes the kind path exactly like a kind- context", func(t *testing.T) {
		s := &stubRunner{}
		s.on("kubectl --context devel@oidc get endpoints",
			`{"subsets":[{"addresses":[{"targetRef":{"name":"pod-x"}}],"ports":[{"port":80}]}]}`, nil)

		fake := &fakeForwardProcess{port: "1111", hasResult: true}

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
