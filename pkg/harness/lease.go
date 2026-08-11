package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// LeaseDuration is how long a tenant claim survives without renewal.
	// Long enough to ride out network blips and a gemaal-service restart
	// (the lock state lives in the cluster, not in any service), short
	// enough that a SIGKILL'd run frees its lane in about a minute and a
	// half rather than a TTL's worth of hours.
	LeaseDuration = 90 * time.Second

	// leaseRenewEvery is the renewal cadence — duration/3, the
	// leader-election convention: two consecutive renewals may fail
	// before the claim is at risk.
	leaseRenewEvery = LeaseDuration / 3

	// leaseTimeLayout is metav1.MicroTime's wire format.
	leaseTimeLayout = "2006-01-02T15:04:05.000000Z07:00"
)

// ErrTenantHeld reports a live claim by someone else. It is a correctness
// stop, not an infrastructure failure: proceeding would race two suites
// over one Helm release pair, and each teardown would uninstall the
// other's live install — observed, twice, before this existed.
type ErrTenantHeld struct {
	Tenant Tenant
	Holder string
}

func (e ErrTenantHeld) Error() string {
	return fmt.Sprintf(
		"tenant %s is held by %q — another suite or agent is using this release; "+
			"set %s to claim your own release name, or wait for the holder to finish "+
			"(an abandoned claim frees itself within %s)",
		e.Tenant, e.Holder, EnvRelease, LeaseDuration)
}

// TenantClaim is a held claim: a coordination.k8s.io Lease named after
// the release, renewed in the background while the suite runs.
//
// The claim is the CORRECTNESS lock for the tenant (efficiency locks
// belong elsewhere): the k8s API is the arbiter, compare-and-swap rides
// on resourceVersion via `kubectl replace`, and expiry needs no janitor
// because absence of renewal IS the release — the dead-hand property,
// with the lease as the truth that survives blips and restarts.
type TenantClaim struct {
	cluster *Cluster
	tenant  Tenant
	holder  string
	cancel  context.CancelFunc
	done    chan struct{}
}

// leaseObject is the subset of coordination.k8s.io/v1 Lease this package
// reads and writes.
type leaseObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name            string            `json:"name"`
		Namespace       string            `json:"namespace"`
		ResourceVersion string            `json:"resourceVersion,omitempty"`
		Labels          map[string]string `json:"labels,omitempty"`
	} `json:"metadata"`
	Spec struct {
		HolderIdentity       string `json:"holderIdentity"`
		LeaseDurationSeconds int    `json:"leaseDurationSeconds"`
		AcquireTime          string `json:"acquireTime,omitempty"`
		RenewTime            string `json:"renewTime,omitempty"`
	} `json:"spec"`
}

func leaseName(t Tenant) string { return "gemaal-claim-" + t.Release }

// errLeaseUnavailable marks lease-API trouble that fails OPEN: the run
// proceeds unprotected (with a loud warning) instead of being blocked by
// coordination infrastructure it can live without.
var errLeaseUnavailable = errors.New("lease unavailable")

// errLeaseNotFound marks a cleanly-absent lease — the fresh-claim path.
var errLeaseNotFound = errors.New("lease not found")

// ClaimTenant acquires the tenant's claim for holder, taking over an
// expired one, and starts the background renewal. Returns ErrTenantHeld
// when a LIVE claim belongs to someone else.
// A nil, nil return means the claim was UNAVAILABLE (not denied): the
// caller proceeds unprotected, which is the pre-claim status quo.
func (c *Cluster) ClaimTenant(ctx context.Context, tenant Tenant, holder string) (*TenantClaim, error) {
	if err := c.acquireLease(ctx, tenant, holder); err != nil {
		if errors.Is(err, errLeaseUnavailable) {
			return nil, nil
		}

		return nil, err
	}

	renewCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	claim := &TenantClaim{
		cluster: c,
		tenant:  tenant,
		holder:  holder,
		cancel:  cancel,
		done:    make(chan struct{}),
	}

	go claim.renewLoop(renewCtx)

	return claim, nil
}

// Release stops renewal and deletes the lease when still ours. Freeing
// the lane on a clean exit is deliberate even when the install is kept
// (EnvKeep): "keep" keeps the RELEASES; ownership of the lane ends with
// the run, and the next claimant may upgrade-install over the standing
// pair.
func (cl *TenantClaim) Release(ctx context.Context) {
	cl.cancel()
	<-cl.done

	current, err := cl.cluster.getLease(ctx, cl.tenant)
	if err != nil || current.Spec.HolderIdentity != cl.holder {
		return // gone, stolen after expiry, or unreadable — nothing of ours to free
	}

	_ = cl.cluster.runner().Run(ctx, cl.cluster.kubectlArgs(
		"delete", "lease", leaseName(cl.tenant),
		"--namespace", cl.tenant.Namespace,
		"--ignore-not-found",
	)...)
}

func (cl *TenantClaim) renewLoop(ctx context.Context) {
	defer close(cl.done)

	ticker := time.NewTicker(leaseRenewEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := cl.cluster.renewLease(ctx, cl.tenant, cl.holder); err != nil {
				// A failed renewal is survivable (two more fit inside the
				// duration); a STOLEN lease is not — someone treated us as
				// expired, and every further helm operation races theirs.
				var held ErrTenantHeld
				if errors.As(err, &held) {
					fmt.Fprintf(os.Stderr,
						"harness: tenant claim LOST to %q — the suite keeps running but is no longer protected\n",
						held.Holder)

					return
				}

				fmt.Fprintf(os.Stderr, "harness: claim renewal failed (will retry): %v\n", err)
			}
		}
	}
}

// acquireLease creates the lease, or takes over an expired one; a live
// foreign claim is ErrTenantHeld. Lease-API failures other than
// conflicts fail OPEN with a warning: the claim is protection for the
// local many-agents case, and a cluster where leases are unavailable
// should degrade to today's behavior, not brick every suite.
func (c *Cluster) acquireLease(ctx context.Context, tenant Tenant, holder string) error {
	current, err := c.getLease(ctx, tenant)

	switch {
	case err == nil:
		expired := true

		if ts, perr := time.Parse(leaseTimeLayout, current.Spec.RenewTime); perr == nil {
			dur := time.Duration(current.Spec.LeaseDurationSeconds) * time.Second
			expired = time.Since(ts) > dur
		}

		if !expired && current.Spec.HolderIdentity != holder {
			return ErrTenantHeld{Tenant: tenant, Holder: current.Spec.HolderIdentity}
		}

		// Ours already, or expired: take it over. `kubectl replace`
		// carries resourceVersion, so a concurrent takeover loses the
		// compare-and-swap instead of silently double-claiming.
		return c.writeLease(ctx, tenant, holder, current.Metadata.ResourceVersion, "replace")
	case errors.Is(err, errLeaseNotFound):
		return c.writeLease(ctx, tenant, holder, "", "create")
	default:
		fmt.Fprintf(os.Stderr,
			"harness: tenant claim unavailable (%v) — proceeding UNPROTECTED against concurrent agents\n", err)

		return errLeaseUnavailable
	}
}

func (c *Cluster) renewLease(ctx context.Context, tenant Tenant, holder string) error {
	current, err := c.getLease(ctx, tenant)
	if err != nil {
		return err
	}

	if current.Spec.HolderIdentity != holder {
		return ErrTenantHeld{Tenant: tenant, Holder: current.Spec.HolderIdentity}
	}

	return c.writeLease(ctx, tenant, holder, current.Metadata.ResourceVersion, "replace")
}

func (c *Cluster) getLease(ctx context.Context, tenant Tenant) (*leaseObject, error) {
	// --ignore-not-found turns absence into exit 0 with EMPTY output.
	// This is the ONLY reliable absence signal available here: kubectl
	// prints "NotFound" on stderr, which ExecRunner streams to the
	// terminal rather than into the error — err.Error() is a bare
	// "exit status 1", and matching on it shipped a claim that never
	// engaged (every fresh claim read as "unavailable" and failed open).
	out, err := c.runner().Output(ctx, c.kubectlArgs(
		"get", "lease", leaseName(tenant),
		"--namespace", tenant.Namespace,
		"--ignore-not-found",
		"-o", "json",
	)...)
	if err != nil {
		return nil, err
	}

	if strings.TrimSpace(out) == "" {
		return nil, errLeaseNotFound
	}

	var lease leaseObject
	if err := json.Unmarshal([]byte(out), &lease); err != nil {
		return nil, fmt.Errorf("parse lease: %w", err)
	}

	return &lease, nil
}

func (c *Cluster) writeLease(ctx context.Context, tenant Tenant, holder, resourceVersion, verb string) error {
	now := time.Now().UTC().Format(leaseTimeLayout)

	var lease leaseObject
	lease.APIVersion = "coordination.k8s.io/v1"
	lease.Kind = "Lease"
	lease.Metadata.Name = leaseName(tenant)
	lease.Metadata.Namespace = tenant.Namespace
	lease.Metadata.ResourceVersion = resourceVersion
	lease.Metadata.Labels = map[string]string{"app.kubernetes.io/managed-by": "gemaal-harness"}
	lease.Spec.HolderIdentity = holder
	lease.Spec.LeaseDurationSeconds = int(LeaseDuration / time.Second)
	lease.Spec.AcquireTime = now
	lease.Spec.RenewTime = now

	data, err := json.Marshal(lease)
	if err != nil {
		return fmt.Errorf("marshal lease: %w", err)
	}

	dir, err := os.MkdirTemp("", "gemaal-lease-*")
	if err != nil {
		return err
	}

	defer func() { _ = os.RemoveAll(dir) }()

	path := filepath.Join(dir, "lease.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}

	return c.runner().Run(ctx, c.kubectlArgs(verb, "-f", path)...)
}

// DefaultHolder identifies this process for claim purposes: enough to
// NAME the other side in a conflict, which is what turns "deploy failed"
// into "held by o-tsarev@msa2#41234".
func DefaultHolder() string {
	host, _ := os.Hostname()

	user := os.Getenv("USER")
	if user == "" {
		user = "unknown"
	}

	return fmt.Sprintf("%s@%s#%d", user, host, os.Getpid())
}
