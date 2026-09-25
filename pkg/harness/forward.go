package harness

import (
	"bufio"
	"context"
	"fmt"
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
// backing the Service's ready endpoint, open a kubectl port-forward
// straight to that Pod (never to "svc/…", which would leave the chosen
// Pod invisible to us), and return "http://127.0.0.1:<local port>".
func (c *Cluster) servicePortForwardURL(ctx context.Context, namespace, service string, port int) (string, error) {
	pod, err := c.forwardPodFor(ctx, namespace, service)
	if err != nil {
		return "", err
	}

	argv := c.kubectlArgs("port-forward", "-n", namespace, "pod/"+pod, ":"+strconv.Itoa(port))

	start := c.forwardStart
	if start == nil {
		start = execForwardStart
	}

	proc, err := start(ctx, argv)
	if err != nil {
		return "", fmt.Errorf("port-forward %s/%s (pod/%s): %w", namespace, service, pod, err)
	}

	localPort, err := waitForForwardReady(proc, c.forwardReadyTimeout())
	if err != nil {
		_ = proc.Close()

		return "", fmt.Errorf("port-forward %s/%s (pod/%s) never became ready: %w", namespace, service, pod, err)
	}

	fw := &PortForward{
		Namespace: namespace,
		Service:   service,
		Pod:       pod,
		LocalAddr: "127.0.0.1:" + localPort,
		proc:      proc,
	}

	c.forwardsMu.Lock()
	c.forwards = append(c.forwards, fw)
	c.forwardsMu.Unlock()

	fmt.Fprintf(os.Stderr, "harness: port-forwarding %s/%s through pod/%s at %s\n",
		namespace, service, pod, fw.LocalAddr)

	return "http://" + fw.LocalAddr, nil
}

// forwardPodFor resolves the Pod backing a Service's first ready
// endpoint — the same Pod kubectl's own "port-forward svc/…" would pick
// internally, resolved up front here so the harness can NAME it.
func (c *Cluster) forwardPodFor(ctx context.Context, namespace, service string) (string, error) {
	out, err := c.runner().Output(ctx, c.kubectlArgs(
		"get", "endpoints", service, "-n", namespace,
		"-o", "jsonpath={.subsets[0].addresses[0].targetRef.name}")...)
	if err != nil {
		return "", fmt.Errorf("resolve pod for service %s/%s: %w", namespace, service, err)
	}

	pod := strings.TrimSpace(out)
	if pod == "" {
		return "", fmt.Errorf("service %s/%s has no ready endpoint to forward to", namespace, service)
	}

	return pod, nil
}

// forwardProcess is the seam a kind tier port-forward's long-running
// subprocess goes through. Run and Output on the Runner interface are
// both synchronous-to-completion, which a port-forward never reaches on
// its own — it runs until killed — so it needs a seam of its own rather
// than reusing Runner.
type forwardProcess interface {
	// Line blocks for the process's next line of stdout, or returns a
	// non-nil error once the process ends or the stream breaks.
	Line() (string, error)

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

// waitForForwardReady reads proc's stdout until the Forwarding line
// appears, the process ends, or timeout elapses.
func waitForForwardReady(proc forwardProcess, timeout time.Duration) (string, error) {
	type result struct {
		port string
		err  error
	}

	ch := make(chan result, 1)

	go func() {
		for {
			line, err := proc.Line()
			if err != nil {
				ch <- result{"", err}

				return
			}

			if port, ok := parseForwardingLine(line); ok {
				ch <- result{port, nil}

				return
			}
		}
	}()

	select {
	case r := <-ch:
		return r.port, r.err
	case <-time.After(timeout):
		return "", fmt.Errorf("timed out after %s waiting for kubectl's Forwarding line", timeout)
	}
}

// execForwardProcess is forwardProcess backed by a real kubectl
// subprocess — the production default.
type execForwardProcess struct {
	cmd   *exec.Cmd
	lines chan string
	errs  chan error

	// waitOnce guards cmd.Wait(), which os/exec panics if called twice.
	// Both the scanner goroutine (on a natural exit) and Close (on a
	// forced kill) reach it; whichever gets there first performs the
	// real call, and the other observes the same cached result.
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
// stdout scanned line-by-line so the Forwarding announcement can be read
// while the process keeps running, stderr left attached to the
// terminal like every other Runner call in this package.
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
		cmd:   cmd,
		lines: make(chan string, 1),
		errs:  make(chan error, 1),
	}

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			p.lines <- scanner.Text()
		}

		if err := scanner.Err(); err != nil {
			_ = p.wait()
			p.errs <- err

			return
		}

		p.errs <- fmt.Errorf("kubectl port-forward exited: %w", p.wait())
	}()

	return p, nil
}

// Line implements forwardProcess.
func (p *execForwardProcess) Line() (string, error) {
	select {
	case l := <-p.lines:
		return l, nil
	case err := <-p.errs:
		return "", err
	}
}

// Close implements forwardProcess: kills the process and reaps it.
// Safe to call more than once.
func (p *execForwardProcess) Close() error {
	var err error

	p.closeOnce.Do(func() {
		if p.cmd.Process != nil {
			err = p.cmd.Process.Kill()
		}

		_ = p.wait()
	})

	return err
}
