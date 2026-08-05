package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// DefaultLabelDomain prefixes the ledger labels; a deployment may brand
// its own (docs/design.md, "The ledger").
const DefaultLabelDomain = "gemaal.io"

// KeepUntilLayout is the label-safe timestamp encoding for keep-until.
// Kubernetes label values forbid ':', so RFC3339 cannot ride a label;
// basic ISO 8601 in UTC is alphanumeric, sortable and still readable in
// `kubectl get secret --show-labels`.
const KeepUntilLayout = "20060102T150405Z"

// DefaultHelmTimeout bounds each helm --wait: enough for a
// finalizer-heavy ring chart, short enough that a wedged install
// surfaces as an error instead of a hung test job.
const DefaultHelmTimeout = 5 * time.Minute

// labelValue is the Kubernetes label-value shape the execution id must
// have to ride the release Secret.
var labelValue = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)

// Labels is the gemaal ledger stamp for one install — what the service
// reads back from the release Secret. Zero-valued fields are omitted
// (an absent ttl means the tier default applies).
type Labels struct {
	// TTL is this tenant's time-to-live from last activity, as a Go
	// duration string ("24h").
	TTL string

	// KeepUntil holds the tenant until an absolute time, whatever the
	// TTL says.
	KeepUntil time.Time

	// ExecutionID tags the test run performing this install — what
	// execution-scoped assertions filter by.
	ExecutionID string
}

// pairs renders the ledger as helm --labels key=value entries, in ledger
// order (ttl, keep-until, execution-id), validating as it goes.
func (l Labels) pairs(domain string) ([]string, error) {
	var pairs []string

	if l.TTL != "" {
		if _, err := time.ParseDuration(l.TTL); err != nil {
			return nil, fmt.Errorf("ttl label: %w", err)
		}

		pairs = append(pairs, domain+"/ttl="+l.TTL)
	}

	if !l.KeepUntil.IsZero() {
		pairs = append(pairs, domain+"/keep-until="+l.KeepUntil.UTC().Format(KeepUntilLayout))
	}

	if l.ExecutionID != "" {
		if len(l.ExecutionID) > 63 || !labelValue.MatchString(l.ExecutionID) {
			return nil, fmt.Errorf("execution id %q is not a valid label value", l.ExecutionID)
		}

		pairs = append(pairs, domain+"/execution-id="+l.ExecutionID)
	}

	return pairs, nil
}

// DefaultExecutionID derives an execution id for this run: the CI run
// coordinates when present (matching the CI release name), otherwise a
// timestamp — distinct across re-runs in the same standing tenant, which
// is exactly what execution-scoped assertions need.
func DefaultExecutionID(now time.Time) string {
	if r, ok := ciRelease(); ok {
		return r
	}

	return "local-" + now.UTC().Format(KeepUntilLayout)
}

// Cluster is the harness's toolbox for one cluster: client-side helm
// installs stamped with the ledger labels (helm >= 3.13 --labels, on the
// release Secret), ring-pair ordering, and the kubectl-side helpers
// tests need. It installs into a namespace; it never creates one.
type Cluster struct {
	// Kubecontext targets a kubeconfig context (empty = current).
	Kubecontext string

	// Kubeconfig overrides the ambient kubeconfig for helm and kubectl.
	Kubeconfig string

	// Runner executes helm and kubectl; ExecRunner when nil.
	Runner Runner

	// HelmTimeout bounds each helm --wait; DefaultHelmTimeout when zero.
	HelmTimeout time.Duration

	// RolloutTimeout bounds each Deployment rollout wait;
	// DefaultRolloutTimeout when zero.
	RolloutTimeout time.Duration

	// LabelDomain prefixes the ledger labels; DefaultLabelDomain when
	// empty.
	LabelDomain string
}

func (c *Cluster) runner() Runner {
	if c.Runner != nil {
		return c.Runner
	}

	return ExecRunner{}
}

func (c *Cluster) helmTimeout() time.Duration {
	if c.HelmTimeout > 0 {
		return c.HelmTimeout
	}

	return DefaultHelmTimeout
}

func (c *Cluster) labelDomain() string {
	if c.LabelDomain != "" {
		return c.LabelDomain
	}

	return DefaultLabelDomain
}

// helmArgs builds a helm invocation with the cluster's global flags.
func (c *Cluster) helmArgs(args ...string) []string {
	argv := append([]string{"helm"}, args...)

	if c.Kubecontext != "" {
		argv = append(argv, "--kube-context", c.Kubecontext)
	}

	if c.Kubeconfig != "" {
		argv = append(argv, "--kubeconfig", c.Kubeconfig)
	}

	return argv
}

// kubectlArgs builds a kubectl invocation with the cluster's global
// flags.
func (c *Cluster) kubectlArgs(args ...string) []string {
	argv := []string{"kubectl"}

	if c.Kubecontext != "" {
		argv = append(argv, "--context", c.Kubecontext)
	}

	if c.Kubeconfig != "" {
		argv = append(argv, "--kubeconfig", c.Kubeconfig)
	}

	return append(argv, args...)
}

// Install describes one helm release to install.
type Install struct {
	// Release names the release — usually the tenant's Release, or its
	// InfraRelease companion.
	Release string

	// Chart is a local path, a packaged .tgz, or an OCI ref.
	Chart string

	// ValuesFiles pass through as helm --values, in order (later files
	// win under helm's own precedence).
	ValuesFiles []string

	// Set passes through as helm --set.
	Set []string

	// Labels is the ledger stamp for this release.
	Labels Labels
}

// Install runs `helm upgrade --install --wait` with the ledger labels
// stamped on the release Secret. The release name is bounds-asserted
// first — failing here beats failing inside helm.
func (c *Cluster) Install(ctx context.Context, namespace string, in Install) error {
	if err := validName(in.Release, "release", MaxReleaseLen); err != nil {
		return err
	}

	labels, err := in.Labels.pairs(c.labelDomain())
	if err != nil {
		return err
	}

	argv := c.helmArgs("upgrade", "--install", in.Release, in.Chart, "--namespace", namespace)
	argv = append(argv, "--wait", "--timeout", c.helmTimeout().String())

	if len(labels) > 0 {
		argv = append(argv, "--labels", strings.Join(labels, ","))
	}

	for _, f := range in.ValuesFiles {
		argv = append(argv, "--values", f)
	}

	for _, s := range in.Set {
		argv = append(argv, "--set", s)
	}

	return c.runner().Run(ctx, argv...)
}

// Uninstall removes one release, waiting for its resources to go. A
// release that is not there is not an error (--ignore-not-found), so
// uninstalling a partial install is safe.
func (c *Cluster) Uninstall(ctx context.Context, namespace, release string) error {
	argv := c.helmArgs("uninstall", release, "--namespace", namespace)
	argv = append(argv, "--ignore-not-found", "--wait", "--timeout", c.helmTimeout().String())

	return c.runner().Run(ctx, argv...)
}

// InfraRelease returns the ring2 companion release name for an app
// release: "<release>-infra" — the derivation is fixed so the pair can
// never disagree about its own name.
func InfraRelease(release string) string { return release + "-infra" }

// Pair describes a ring-pair install: the infrastructure chart (ring2)
// and the application chart (ring3), sharing values and ledger labels.
// Release names derive from the tenant — app = tenant.Release, infra =
// InfraRelease of it — and are not configurable separately.
type Pair struct {
	// InfraChart is the ring2 chart (databases, queues, claims).
	InfraChart string

	// AppChart is the ring3 chart (the application).
	AppChart string

	// ValuesFiles pass through to BOTH releases as helm --values.
	ValuesFiles []string

	// Set passes through to both releases as helm --set.
	Set []string

	// Labels is the ledger stamp, applied to both releases.
	Labels Labels
}

// InstallPair installs ring2 before ring3 — the application's
// pre-install hooks (migrations) need the infrastructure standing.
func (c *Cluster) InstallPair(ctx context.Context, tenant Tenant, p Pair) error {
	installs := []Install{
		{Release: InfraRelease(tenant.Release), Chart: p.InfraChart, ValuesFiles: p.ValuesFiles, Set: p.Set, Labels: p.Labels},
		{Release: tenant.Release, Chart: p.AppChart, ValuesFiles: p.ValuesFiles, Set: p.Set, Labels: p.Labels},
	}

	for i := range installs {
		if err := c.Install(ctx, tenant.Namespace, installs[i]); err != nil {
			return err
		}
	}

	return nil
}

// UninstallPair uninstalls in reverse ring order — the application
// first, so it never outlives the infrastructure it is connected to.
// Both uninstalls are attempted even when the first fails; the errors
// come back joined.
func (c *Cluster) UninstallPair(ctx context.Context, tenant Tenant) error {
	return errors.Join(
		c.Uninstall(ctx, tenant.Namespace, tenant.Release),
		c.Uninstall(ctx, tenant.Namespace, InfraRelease(tenant.Release)),
	)
}

// ClusterValuesFile writes the opaque per-cluster values from
// gemaal.yaml to a temp file under the top-level `gemaal:` key — charts
// read them as .Values.gemaal. Hand it to helm as the FIRST --values so
// later files and --set still win under helm's own precedence. The
// caller removes the file.
func ClusterValuesFile(values map[string]any) (string, error) {
	data, err := yaml.Marshal(map[string]any{"gemaal": values})
	if err != nil {
		return "", fmt.Errorf("marshal gemaal values: %w", err)
	}

	tmp, err := os.CreateTemp("", "gemaal-values-*.yaml")
	if err != nil {
		return "", err
	}

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return "", err
	}

	return tmp.Name(), tmp.Close()
}
