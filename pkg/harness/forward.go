package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultForwardReadyTimeout bounds how long ServiceURL waits for a kind
// tier port-forward's "Forwarding from" line before giving up.
const DefaultForwardReadyTimeout = 15 * time.Second

func (c *Cluster) forwardReadyTimeout() time.Duration {
	if c.ForwardReadyTimeout > 0 {
		return c.ForwardReadyTimeout
	}

	return DefaultForwardReadyTimeout
}

// PortForward is a live kubectl port-forward the kind tier's ServiceURL
// opened, forwarding to the exact Pod it resolved (never the bare
// Service) — so a request failure through it can be attributed to that
// Pod rather than reading as a mystery service failure when a pod
// restarts mid-test.
type PortForward struct {
	// Namespace, Service and Pod name what this forward reaches.
	Namespace, Service, Pod string

	// LocalAddr is "127.0.0.1:<port>", the local end ServiceURL's
	// returned URL points at.
	LocalAddr string

	proc      forwardProcess
	closeOnce sync.Once
}

// Close stops the forward. Safe to call more than once.
func (f *PortForward) Close() error {
	var err error

	f.closeOnce.Do(func() { err = f.proc.Close() })

	return err
}

// Err wraps an error observed while talking through this forward with
// the Pod it was forwarding to — the harness's answer to "which pod": a
// pod that restarted mid-test otherwise reads as a bare service bug.
func (f *PortForward) Err(err error) error {
	return fmt.Errorf("through port-forward %s/%s -> pod/%s (%s): %w",
		f.Namespace, f.Service, f.Pod, f.LocalAddr, err)
}

// CloseForwards stops every port-forward this Cluster opened (the kind
// tier's ServiceURL). A caller's t.Cleanup(cluster.CloseForwards) is the
// direct equivalent of the per-request forwards a shared-tier caller
// never needed; harness.Run also calls it after m.Run() for the
// TestMain-bracketed lane, so a suite that never touches this method
// still cleans up. Idempotent, and a no-op on the shared tier, where
// nothing was ever opened.
func (c *Cluster) CloseForwards() {
	c.forwardsMu.Lock()
	forwards := c.forwards
	c.forwards = nil
	c.forwardsMu.Unlock()

	for _, f := range forwards {
		_ = f.Close()
	}
}

// ForwardFor returns the most recently opened forward to (namespace,
// service), if this Cluster has one open — the lookup a caller uses to
// enrich its OWN error (via PortForward.Err) when a request made
// through ServiceURL's returned URL fails later, since ServiceURL
// itself returns a bare URL to keep the shared-tier call site
// unchanged.
func (c *Cluster) ForwardFor(namespace, service string) (*PortForward, bool) {
	c.forwardsMu.Lock()
	defer c.forwardsMu.Unlock()

	for i := len(c.forwards) - 1; i >= 0; i-- {
		if c.forwards[i].Namespace == namespace && c.forwards[i].Service == service {
			return c.forwards[i], true
		}
	}

	return nil, false
}

// servicePortForwardURL is the kind tier's ServiceURL: resolve the Pod
// and container port backing the Service's ready endpoint, open a
// kubectl port-forward straight to that Pod (never to "svc/…", which
// would leave the chosen Pod invisible to us), and return
// "http://127.0.0.1:<local port>".
//
// The subprocess is started under a context DETACHED from ctx
// (context.WithoutCancel): ServiceURL's returned URL is meant to outlive
// this call exactly like the shared tier's ClusterIP does, so a caller
// that cancels ctx right after ServiceURL returns (a common
// WithTimeout-around-the-call pattern) must not silently kill the
// forward under only the kind tier. ctx itself still bounds the
// READINESS wait below — a caller whose ctx is already done should not
// wait the full timeout for a forward that will be torn down anyway.
func (c *Cluster) servicePortForwardURL(ctx context.Context, namespace, service string, port int) (string, error) {
	target, err := c.resolveForwardTarget(ctx, namespace, service, port)
	if err != nil {
		return "", err
	}

	argv := c.kubectlArgs("port-forward", "-n", namespace, "pod/"+target.pod, ":"+strconv.Itoa(target.port))

	start := c.forwardStart
	if start == nil {
		start = execForwardStart
	}

	proc, err := start(context.WithoutCancel(ctx), argv)
	if err != nil {
		return "", fmt.Errorf("port-forward %s/%s (pod/%s): %w", namespace, service, target.pod, err)
	}

	readyCtx, cancel := context.WithTimeout(ctx, c.forwardReadyTimeout())
	defer cancel()

	localPort, err := proc.Ready(readyCtx)
	if err != nil {
		_ = proc.Close()

		return "", fmt.Errorf("port-forward %s/%s (pod/%s) never became ready: %w", namespace, service, target.pod, err)
	}

	fw := &PortForward{
		Namespace: namespace,
		Service:   service,
		Pod:       target.pod,
		LocalAddr: "127.0.0.1:" + localPort,
		proc:      proc,
	}

	c.forwardsMu.Lock()
	c.forwards = append(c.forwards, fw)
	c.forwardsMu.Unlock()

	fmt.Fprintf(os.Stderr, "harness: port-forwarding %s/%s (pod/%s:%d) at %s\n",
		namespace, service, target.pod, target.port, fw.LocalAddr)

	return "http://" + fw.LocalAddr, nil
}

// forwardTarget is what ServiceURL resolves before opening a forward:
// the exact Pod behind the Service's first ready endpoint, and the
// endpoint's own resolved container port. The container port is the
// SAME number as the Service port ONLY when the Service's targetPort
// equals its port; a Service with e.g. `port: 8080, targetPort: http`
// (or any `port != targetPort`) needs the endpoint's own numeric port,
// never the Service port ServiceURL was called with.
type forwardTarget struct {
	pod  string
	port int
}

// endpointPort is the subset of a v1.EndpointPort this file reads.
type endpointPort struct {
	Name string `json:"name"`
	Port int    `json:"port"`
}

// resolveForwardTarget resolves the Pod and container port a kind-tier
// forward must dial, from the Service's v1 Endpoints object.
//
// Endpoints (not the newer EndpointSlices) on purpose: Kubernetes
// itself resolves a Service's targetPort — numeric or named — into the
// endpoint's own numeric container port when it POPULATES Endpoints, so
// reading Endpoints already answers "which real port" with no
// name-to-container-port arithmetic of our own to get wrong.
// EndpointSlices carry the same resolved numbers but split them across
// a LIST of objects selected by a "kubernetes.io/service-name" label
// and a per-slice `endpoints[].conditions.ready` — a second call and a
// slice-picking policy this harness does not otherwise need. Endpoints
// is deprecated, not removed, and every kind version this tier targets
// still serves it; if that stops being true, the two-line description
// above is what changes, not the shape of this function.
func (c *Cluster) resolveForwardTarget(ctx context.Context, namespace, service string, port int) (forwardTarget, error) {
	out, err := c.runner().Output(ctx, c.kubectlArgs("get", "endpoints", service, "-n", namespace, "-o", "json")...)
	if err != nil {
		return forwardTarget{}, fmt.Errorf("resolve endpoints for service %s/%s: %w", namespace, service, err)
	}

	var endpoints struct {
		Subsets []struct {
			Addresses []struct {
				TargetRef struct {
					Name string `json:"name"`
				} `json:"targetRef"`
			} `json:"addresses"`
			Ports []endpointPort `json:"ports"`
		} `json:"subsets"`
	}

	if err := json.Unmarshal([]byte(out), &endpoints); err != nil {
		return forwardTarget{}, fmt.Errorf("parse endpoints for service %s/%s: %w", namespace, service, err)
	}

	for _, subset := range endpoints.Subsets {
		if len(subset.Addresses) == 0 || subset.Addresses[0].TargetRef.Name == "" {
			continue
		}

		targetPort, err := c.resolveSubsetPort(ctx, namespace, service, port, subset.Ports)
		if err != nil {
			return forwardTarget{}, err
		}

		return forwardTarget{pod: subset.Addresses[0].TargetRef.Name, port: targetPort}, nil
	}

	return forwardTarget{}, fmt.Errorf("service %s/%s has no ready endpoint to forward to", namespace, service)
}

// resolveSubsetPort picks the endpoint port matching the Service port
// requested. A single-port subset needs no name correlation at all —
// the only shape an unnamed Service port can produce, and Kubernetes
// already resolved it (numeric or named targetPort alike) to the right
// container port. A multi-port subset requires matching by NAME, since
// multiple entries are otherwise indistinguishable — Kubernetes requires
// every port to be named once a Service carries more than one.
func (c *Cluster) resolveSubsetPort(ctx context.Context, namespace, service string, port int, subsetPorts []endpointPort) (int, error) {
	if len(subsetPorts) == 0 {
		return 0, fmt.Errorf("service %s/%s endpoint carries no ports", namespace, service)
	}

	if len(subsetPorts) == 1 {
		return subsetPorts[0].Port, nil
	}

	name, err := c.servicePortName(ctx, namespace, service, port)
	if err != nil {
		return 0, err
	}

	for _, p := range subsetPorts {
		if p.Name == name {
			return p.Port, nil
		}
	}

	return 0, fmt.Errorf("service %s/%s port %d (name %q) has no matching endpoint port", namespace, service, port, name)
}

// servicePortName reads the Service spec to find the NAME of the port
// whose number is port — the correlation key resolveSubsetPort needs
// for a multi-port Service, where Endpoints' own port entries are
// otherwise indistinguishable.
func (c *Cluster) servicePortName(ctx context.Context, namespace, service string, port int) (string, error) {
	out, err := c.runner().Output(ctx, c.kubectlArgs("get", "svc", service, "-n", namespace, "-o", "json")...)
	if err != nil {
		return "", fmt.Errorf("resolve service %s/%s: %w", namespace, service, err)
	}

	var svc struct {
		Spec struct {
			Ports []struct {
				Name string `json:"name"`
				Port int    `json:"port"`
			} `json:"ports"`
		} `json:"spec"`
	}

	if err := json.Unmarshal([]byte(out), &svc); err != nil {
		return "", fmt.Errorf("parse service %s/%s: %w", namespace, service, err)
	}

	for _, p := range svc.Spec.Ports {
		if p.Port == port {
			return p.Name, nil
		}
	}

	return "", fmt.Errorf("service %s/%s carries no port %d", namespace, service, port)
}

// forwardProcess is the seam a kind tier port-forward's long-running
// subprocess goes through. Run and Output on the Runner interface are
// both synchronous-to-completion, which a port-forward never reaches on
// its own — it runs until killed — so it needs a seam of its own rather
// than reusing Runner.
type forwardProcess interface {
	// Ready blocks until kubectl's "Forwarding from" line has been seen
	// (returning the local port), the process ends before announcing
	// one, or ctx is done. Whichever way it returns, the process's
	// stdout keeps being drained for the rest of the process's life —
	// see execForwardProcess's drainForwardOutput — so Ready is called
	// exactly once per process; nothing further needs reading from it.
	Ready(ctx context.Context) (port string, err error)

	// Close stops the process. Safe to call more than once.
	Close() error
}

// forwardStarter starts a forwardProcess for argv.
type forwardStarter func(ctx context.Context, argv []string) (forwardProcess, error)

// forwardingLinePattern matches kubectl port-forward's readiness
// announcement: "Forwarding from 127.0.0.1:<port> -> <remote>" (kubectl
// prints the [::1] IPv6 twin alongside it; only the IPv4 line is used).
var forwardingLinePattern = regexp.MustCompile(`^Forwarding from 127\.0\.0\.1:(\d+) ->`)

// parseForwardingLine extracts the local port from one line of kubectl
// port-forward's stdout, or reports that the line is not it (kubectl
// also prints "Handling connection for …" lines per request, which must
// not be mistaken for readiness).
func parseForwardingLine(line string) (port string, ok bool) {
	m := forwardingLinePattern.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return "", false
	}

	return m[1], true
}

// forwardReady is the one-shot result execForwardProcess's drain
// goroutine delivers over its ready channel.
type forwardReady struct {
	port string
	err  error
}

// execForwardProcess is forwardProcess backed by a real kubectl
// subprocess — the production default.
type execForwardProcess struct {
	cmd   *exec.Cmd
	ready chan forwardReady // buffered 1, written exactly once

	// drained closes once drainForwardOutput returns — after stdout
	// EOFs (the process exited or was killed) AND wait() has already
	// been called from inside that same goroutine. Close blocks on it
	// so Close is fully synchronous: kill, then let the drain finish
	// reading before reaping, per os/exec's own documented order (Wait
	// must not run concurrently with reads from the pipe it owns).
	drained chan struct{}

	// waitOnce guards cmd.Wait(), which os/exec panics if called twice.
	waitOnce sync.Once
	waitErr  error

	closeOnce sync.Once
}

// wait calls cmd.Wait() exactly once, however many callers reach it.
func (p *execForwardProcess) wait() error {
	p.waitOnce.Do(func() { p.waitErr = p.cmd.Wait() })

	return p.waitErr
}

// execForwardStart is the real forwardStarter: `kubectl port-forward`,
// with its stdout drained continuously for the process's whole life
// (see drainForwardOutput) so a long test hammering the forward with
// connections — each one printing a "Handling connection for …" line —
// never fills the pipe and wedges kubectl. stderr is left attached to
// the terminal like every other Runner call in this package.
func execForwardStart(ctx context.Context, argv []string) (forwardProcess, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}

	p := &execForwardProcess{
		cmd:     cmd,
		ready:   make(chan forwardReady, 1),
		drained: make(chan struct{}),
	}

	go func() {
		defer close(p.drained)
		drainForwardOutput(stdout, p.ready, p.wait)
	}()

	return p, nil
}

// drainForwardOutput reads stdout line by line for the WHOLE life of
// the process: the first line matching kubectl's "Forwarding from"
// announcement is delivered once on ready, and every line after that —
// including the unbounded stream of "Handling connection for …" lines
// kubectl prints per request — is read and discarded, which is what
// keeps the pipe from ever filling. wait is called once stdout EOFs
// (the process exited or was killed), from this same goroutine and
// only after every read has completed, satisfying os/exec's own
// ordering requirement for Wait.
func drainForwardOutput(stdout io.Reader, ready chan<- forwardReady, wait func() error) {
	scanner := bufio.NewScanner(stdout)

	found := false

	for scanner.Scan() {
		if found {
			continue // drain and discard — see the doc comment above
		}

		if port, ok := parseForwardingLine(scanner.Text()); ok {
			found = true
			ready <- forwardReady{port: port}
		}
	}

	scanErr := scanner.Err()
	waitErr := wait()

	if !found {
		if scanErr != nil {
			ready <- forwardReady{err: scanErr}
		} else {
			ready <- forwardReady{err: fmt.Errorf("kubectl port-forward exited before announcing readiness: %w", waitErr)}
		}
	}
}

// Ready implements forwardProcess.
func (p *execForwardProcess) Ready(ctx context.Context) (string, error) {
	select {
	case r := <-p.ready:
		return r.port, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Close implements forwardProcess: kills the process, waits for the
// drain goroutine to finish reaping it, and returns. Safe to call more
// than once.
func (p *execForwardProcess) Close() error {
	var err error

	p.closeOnce.Do(func() {
		if p.cmd.Process != nil {
			err = p.cmd.Process.Kill()
		}

		<-p.drained // the drain goroutine calls wait(); block until it has
	})

	return err
}
