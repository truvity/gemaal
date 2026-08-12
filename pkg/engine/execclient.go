package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// uninstallTimeout is passed to helm uninstall --timeout. Ring2
	// teardown waits on database and bucket finalizers, which are not
	// fast.
	uninstallTimeout = "10m"
)

type (
	// Runner executes one external command and captures stdout. The seam
	// tests inject scripted stubs through.
	Runner interface {
		Output(ctx context.Context, argv ...string) (string, error)
	}

	// ExecRunner is the real Runner: os/exec, stderr folded into the
	// error. Nothing interactive — this runs in a service pod.
	ExecRunner struct{}

	// ExecClient implements KubeClient and HelmClient by shelling out to
	// kubectl and helm. kubectl/helm are the abstraction boundary used
	// everywhere else in this stack — the service deliberately does not
	// link client-go or the Helm SDK, so it sees exactly what an
	// operator running the same command sees. In-cluster both tools use
	// the pod's service account.
	ExecClient struct {
		// Kubecontext targets a kubeconfig context; empty = ambient
		// (in-cluster) config.
		Kubecontext string

		// KubectlBin and HelmBin default to "kubectl" and "helm".
		KubectlBin string
		HelmBin    string

		// Runner defaults to ExecRunner.
		Runner Runner
	}

	// helmListEntry is the subset of `helm list -o json` the engine uses.
	helmListEntry struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Updated   string `json:"updated"`
		Status    string `json:"status"`
	}
)

// helmTimeLayouts covers the formats helm has emitted for "updated".
var helmTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05 -0700 MST",
	// helm v4 repeats the numeric offset where v3 wrote the zone NAME:
	// "+0200 +0200" instead of "+0200 CEST". The fractional-second
	// element is optional to Go's parser, so one layout covers both
	// sub-second and whole-second stamps.
	"2006-01-02 15:04:05.999999999 -0700 -0700",
	time.RFC3339Nano,
	time.RFC3339,
}

// Output implements Runner.
func (ExecRunner) Output(ctx context.Context, argv ...string) (string, error) {
	//nolint:gosec // G204: kubectl/helm are trusted tools, args come from the validated config
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
	}

	return stdout.String(), nil
}

func (c ExecClient) runner() Runner {
	if c.Runner != nil {
		return c.Runner
	}

	return ExecRunner{}
}

func (c ExecClient) kubectl(args ...string) []string {
	bin := c.KubectlBin
	if bin == "" {
		bin = "kubectl"
	}

	argv := []string{bin}
	if c.Kubecontext != "" {
		argv = append(argv, "--context", c.Kubecontext)
	}

	return append(argv, args...)
}

func (c ExecClient) helm(args ...string) []string {
	bin := c.HelmBin
	if bin == "" {
		bin = "helm"
	}

	argv := []string{bin}
	if c.Kubecontext != "" {
		argv = append(argv, "--kube-context", c.Kubecontext)
	}

	return append(argv, args...)
}

// ListTierNamespaces selects namespaces by the tier LABEL at the API
// server — `-l "<label> in (a,b)"`. A namespace without the label
// (gemaal-system, platform namespaces) is never returned, so it is
// structurally out of reach.
func (c ExecClient) ListTierNamespaces(ctx context.Context, label string, tiers []string) ([]Namespace, error) {
	if len(tiers) == 0 {
		return nil, nil
	}

	selector := fmt.Sprintf("%s in (%s)", label, strings.Join(tiers, ","))

	out, err := c.runner().Output(ctx, c.kubectl("get", "namespaces", "-l", selector, "-o", "json")...)
	if err != nil {
		return nil, err
	}

	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}

	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parse namespace list: %w", err)
	}

	namespaces := make([]Namespace, 0, len(list.Items))
	for _, item := range list.Items {
		namespaces = append(namespaces, Namespace{
			Name: item.Metadata.Name,
			Tier: item.Metadata.Labels[label],
		})
	}

	return namespaces, nil
}

// ListReleases lists the releases in one namespace. --all includes
// failed/pending releases: a failed release still owns its resources,
// so its tenant is live.
func (c ExecClient) ListReleases(ctx context.Context, namespace string) ([]Release, error) {
	out, err := c.runner().Output(ctx, c.helm(
		"list", "--namespace", namespace,
		"--all", "--max", "0", "--output", "json",
	)...)
	if err != nil {
		return nil, err
	}

	var entries []helmListEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		return nil, fmt.Errorf("parse helm list output for namespace %q: %w", namespace, err)
	}

	releases := make([]Release, 0, len(entries))

	for _, entry := range entries {
		release := Release{
			Namespace: namespace,
			Name:      entry.Name,
			Status:    entry.Status,
		}

		// A timestamp that refuses to parse must not fail the whole
		// listing — one broken stamp would stall planning for the entire
		// estate. The release is returned with the problem recorded and
		// the engine HOLDS its tenant (never delete what you cannot
		// read). Exec and decode failures above stay fatal: those mean
		// the listing itself is untrustworthy.
		if updated, err := parseHelmTime(entry.Updated); err != nil {
			release.UpdatedProblem = err.Error()
		} else {
			release.Updated = updated
		}

		releases = append(releases, release)
	}

	return releases, nil
}

// Uninstall removes one release. --ignore-not-found makes the operation
// idempotent: a plan may be minutes old by the time it is applied.
func (c ExecClient) Uninstall(ctx context.Context, namespace, release string) error {
	_, err := c.runner().Output(ctx, c.helm(
		"uninstall", release,
		"--namespace", namespace,
		"--ignore-not-found",
		"--wait",
		"--timeout", uninstallTimeout,
	)...)

	return err
}

// ReleaseSecrets lists the helm-owned Secrets in a namespace and keeps,
// per release, the one with the highest revision — the Secret whose
// labels are the tenant's current ledger.
func (c ExecClient) ReleaseSecrets(ctx context.Context, namespace string) ([]ReleaseSecret, error) {
	out, err := c.runner().Output(ctx, c.kubectl(
		"get", "secrets",
		"--namespace", namespace,
		"-l", "owner=helm",
		"-o", "json",
	)...)
	if err != nil {
		return nil, err
	}

	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}

	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parse secret list for namespace %q: %w", namespace, err)
	}

	newest := map[string]ReleaseSecret{}

	for _, item := range list.Items {
		release := item.Metadata.Labels["name"]
		if release == "" {
			continue
		}

		version, _ := strconv.Atoi(item.Metadata.Labels["version"])

		current, seen := newest[release]
		if !seen || version > current.Version {
			newest[release] = ReleaseSecret{
				Namespace: namespace,
				Name:      item.Metadata.Name,
				Release:   release,
				Version:   version,
				Labels:    item.Metadata.Labels,
			}
		}
	}

	secrets := make([]ReleaseSecret, 0, len(newest))
	for _, s := range newest {
		secrets = append(secrets, s)
	}

	sort.Slice(secrets, func(i, j int) bool { return secrets[i].Release < secrets[j].Release })

	return secrets, nil
}

// LabelSecret sets labels via `kubectl label --overwrite` — the
// mechanically simplest keep-until write (a helm upgrade round-trip
// would need chart and values the watcher deliberately does not have).
func (c ExecClient) LabelSecret(ctx context.Context, namespace, secret string, labels map[string]string) error {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	args := []string{"label", "secret", secret, "--namespace", namespace, "--overwrite"}
	for _, k := range keys {
		args = append(args, k+"="+labels[k])
	}

	_, err := c.runner().Output(ctx, c.kubectl(args...)...)

	return err
}

// parseHelmTime is tolerant of the layouts helm has used for "updated".
// An unparseable timestamp is an error, never "assume it is old": the
// age is the only thing standing between a release and a TTL deletion.
// The caller records the error per release (Release.UpdatedProblem) so
// the tenant is held instead of the whole listing failing.
func parseHelmTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fmt.Errorf("empty helm updated timestamp")
	}

	for _, layout := range helmTimeLayouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}

	return time.Time{}, fmt.Errorf("unrecognized helm updated timestamp %q", value)
}

// Lease reads one coordination.k8s.io Lease. --ignore-not-found turns
// absence into exit 0 with EMPTY output — the only reliable absence
// signal here: kubectl prints NotFound on stderr, which the runner
// streams rather than folds into the error (the harness's claim code
// shipped a lock that never engaged by matching error text; see
// gemaal#17).
func (c ExecClient) Lease(ctx context.Context, namespace, name string) (Lease, error) {
	out, err := c.runner().Output(ctx, c.kubectl(
		"get", "lease", name,
		"--namespace", namespace,
		"--ignore-not-found",
		"-o", "json",
	)...)
	if err != nil {
		return Lease{}, err
	}

	if strings.TrimSpace(out) == "" {
		return Lease{}, nil
	}

	var lease struct {
		Spec struct {
			HolderIdentity       string `json:"holderIdentity"`
			LeaseDurationSeconds int    `json:"leaseDurationSeconds"`
			RenewTime            string `json:"renewTime"`
		} `json:"spec"`
	}

	if err := json.Unmarshal([]byte(out), &lease); err != nil {
		return Lease{}, fmt.Errorf("parse lease %s/%s: %w", namespace, name, err)
	}

	renew, err := time.Parse("2006-01-02T15:04:05.000000Z07:00", lease.Spec.RenewTime)
	if err != nil {
		// An unparseable renewal reads as absent-in-time: the liveness
		// check below treats a zero RenewTime as expired.
		renew = time.Time{}
	}

	return Lease{
		Holder:          lease.Spec.HolderIdentity,
		RenewTime:       renew,
		DurationSeconds: lease.Spec.LeaseDurationSeconds,
	}, nil
}
