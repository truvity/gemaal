package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultRolloutTimeout bounds each Deployment rollout wait.
const DefaultRolloutTimeout = 2 * time.Minute

// DefaultRolloutPollInterval paces WaitForDeploymentsAtVersion's polling
// of each Deployment's own JSON.
const DefaultRolloutPollInterval = 2 * time.Second

func (c *Cluster) rolloutTimeout() time.Duration {
	if c.RolloutTimeout > 0 {
		return c.RolloutTimeout
	}

	return DefaultRolloutTimeout
}

func (c *Cluster) rolloutPollInterval() time.Duration {
	if c.RolloutPollInterval > 0 {
		return c.RolloutPollInterval
	}

	return DefaultRolloutPollInterval
}

// ServiceClusterIP resolves a Service's ClusterIP. Headless and
// not-yet-assigned services are errors — there is nothing to connect to.
func (c *Cluster) ServiceClusterIP(ctx context.Context, namespace, service string) (string, error) {
	out, err := c.runner().Output(ctx,
		c.kubectlArgs("get", "svc", service, "-n", namespace, "-o", "jsonpath={.spec.clusterIP}")...)
	if err != nil {
		return "", fmt.Errorf("get svc %s/%s: %w", namespace, service, err)
	}

	ip := strings.TrimSpace(out)
	if ip == "" || ip == "None" {
		return "", fmt.Errorf("service %s/%s has no ClusterIP (got %q)", namespace, service, ip)
	}

	return ip, nil
}

// ServiceURL renders a URL that reaches a Service, however this
// Cluster's tier reaches things: on the shared tier (the default, every
// existing caller) it is "http://<clusterIP>:<port>" — reachable
// directly because the operator routes the service CIDR to the test
// network (e.g. a tailnet subnet route). On TierKind, where no such
// route exists (a laptop's Docker Desktop cannot route to one at all),
// it opens a kubectl port-forward to the Pod behind the Service instead
// and returns "http://127.0.0.1:<local port>" — see
// servicePortForwardURL and (*Cluster).CloseForwards.
func (c *Cluster) ServiceURL(ctx context.Context, namespace, service string, port int) (string, error) {
	if c.tier() == TierKind {
		return c.servicePortForwardURL(ctx, namespace, service, port)
	}

	ip, err := c.ServiceClusterIP(ctx, namespace, service)
	if err != nil {
		return "", err
	}

	return "http://" + ip + ":" + strconv.Itoa(port), nil
}

// ReleaseDeployments lists the Deployments owned by any of the given
// releases. A Deployment belongs to a release when the
// meta.helm.sh/release-name annotation helm stamps names it, or, when
// that annotation is absent, when the app.kubernetes.io/instance label
// every chart carries names it instead — the only signal left on a
// release a GitOps controller rendered with "helm template" and applied
// directly, never running "helm install/upgrade" to set the annotation.
// The annotation is authoritative when present: a Deployment annotated
// for one release is never matched by a label naming another.
func (c *Cluster) ReleaseDeployments(ctx context.Context, namespace string, releases ...string) ([]string, error) {
	out, err := c.runner().Output(ctx,
		c.kubectlArgs("get", "deployments", "-n", namespace, "-o", "json")...)
	if err != nil {
		return nil, fmt.Errorf("list deployments in %s: %w", namespace, err)
	}

	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
				Labels      map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}

	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parse deployment list: %w", err)
	}

	owned := make(map[string]bool, len(releases))
	for _, r := range releases {
		owned[r] = true
	}

	var names []string

	for i := range list.Items {
		if owned[releaseOf(list.Items[i].Metadata.Annotations, list.Items[i].Metadata.Labels)] {
			names = append(names, list.Items[i].Metadata.Name)
		}
	}

	return names, nil
}

// releaseOf resolves the release name an object belongs to: the
// meta.helm.sh/release-name annotation helm stamps when present,
// otherwise the app.kubernetes.io/instance label every chart carries.
func releaseOf(annotations, labels map[string]string) string {
	if name, ok := annotations["meta.helm.sh/release-name"]; ok {
		return name
	}

	return labels["app.kubernetes.io/instance"]
}

// WaitForDeployments waits for every Deployment owned by the releases to
// roll out — belt-and-suspenders after helm --wait, and the whole story
// when a run reuses an install it did not make. Zero owned Deployments
// is an error: a release that deploys nothing is not "ready".
func (c *Cluster) WaitForDeployments(ctx context.Context, namespace string, releases ...string) error {
	names, err := c.ReleaseDeployments(ctx, namespace, releases...)
	if err != nil {
		return err
	}

	if len(names) == 0 {
		return fmt.Errorf("no Deployments found for releases %v in namespace %q", releases, namespace)
	}

	for _, name := range names {
		err := c.runner().Run(ctx, c.kubectlArgs(
			"rollout", "status", "deployment/"+name,
			"-n", namespace,
			"--timeout", c.rolloutTimeout().String())...)
		if err != nil {
			return fmt.Errorf("deployment %s/%s not ready: %w", namespace, name, err)
		}
	}

	return nil
}

// WaitForDeploymentsAtVersion waits for every Deployment owned by the
// releases to complete a rollout to a SPECIFIC version — proved by
// versionLabel=want on the Deployment's OWN pod template — rather than
// whatever spec happens to be live at the moment this is called.
//
// WaitForDeployments (`kubectl rollout status`) proves a Deployment has
// finished rolling out, but not WHICH generation: called before a
// controller has pushed the new spec at all, it sees the OLD generation
// already fully rolled out and returns immediately. That is the exact
// race this closes — e.g. a test-chart Job applied by a GitOps
// controller before it finishes updating the app release's own
// Deployments, which would otherwise let the suite pass against the
// PREVIOUS version's pods.
//
// This polls each Deployment's JSON — the same fields `kubectl rollout
// status` itself reads — until: the pod template carries
// versionLabel=want, status.observedGeneration has caught up with
// metadata.generation, and status.updatedReplicas,
// status.replicas and status.availableReplicas all equal the wanted
// replica count (no old-ReplicaSet pods left terminating, nothing
// unavailable). Bounded by RolloutTimeout; a stuck rollout's error names
// the Deployment and the condition still unmet.
func (c *Cluster) WaitForDeploymentsAtVersion(ctx context.Context, namespace, versionLabel, want string, releases ...string) error {
	names, err := c.ReleaseDeployments(ctx, namespace, releases...)
	if err != nil {
		return err
	}

	if len(names) == 0 {
		return fmt.Errorf("no Deployments found for releases %v in namespace %q", releases, namespace)
	}

	for _, name := range names {
		if err := c.waitForDeploymentAtVersion(ctx, namespace, name, versionLabel, want); err != nil {
			return err
		}
	}

	return nil
}

// deploymentState is the slice of `kubectl get deployment -o json` that
// deploymentRolledOut needs — the same fields `kubectl rollout status`
// itself reads, plus the pod template's own labels for the version
// check nothing else here can do.
type deploymentState struct {
	Metadata struct {
		Generation int64 `json:"generation"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int32 `json:"replicas"`
		Template struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64 `json:"observedGeneration"`
		Replicas           int32 `json:"replicas"`
		UpdatedReplicas    int32 `json:"updatedReplicas"`
		AvailableReplicas  int32 `json:"availableReplicas"`
	} `json:"status"`
}

// wantedReplicas is spec.replicas, or Kubernetes' own default of 1 when
// the field is unset.
func (d deploymentState) wantedReplicas() int32 {
	if d.Spec.Replicas != nil {
		return *d.Spec.Replicas
	}

	return 1
}

// rolledOut reports whether this Deployment has finished rolling out to
// versionLabel=want — every condition `kubectl rollout status` checks,
// plus the version label match that proves it is THIS generation, not
// whatever was already live. A false report carries the one condition
// still unmet, for the timeout error.
func (d deploymentState) rolledOut(versionLabel, want string) (bool, string) {
	if got := d.Spec.Template.Metadata.Labels[versionLabel]; got != want {
		return false, fmt.Sprintf("pod template %s=%q, want %q", versionLabel, got, want)
	}

	if d.Status.ObservedGeneration < d.Metadata.Generation {
		return false, fmt.Sprintf("observedGeneration %d has not caught up with generation %d",
			d.Status.ObservedGeneration, d.Metadata.Generation)
	}

	replicas := d.wantedReplicas()

	if d.Status.UpdatedReplicas != replicas {
		return false, fmt.Sprintf("%d/%d replicas updated", d.Status.UpdatedReplicas, replicas)
	}

	if d.Status.Replicas != replicas {
		return false, fmt.Sprintf("%d/%d replicas present (an old ReplicaSet may still be terminating)",
			d.Status.Replicas, replicas)
	}

	if d.Status.AvailableReplicas != replicas {
		return false, fmt.Sprintf("%d/%d replicas available", d.Status.AvailableReplicas, replicas)
	}

	return true, ""
}

func (c *Cluster) waitForDeploymentAtVersion(ctx context.Context, namespace, name, versionLabel, want string) error {
	deadline := time.Now().Add(c.rolloutTimeout())

	var lastReason string

	for {
		reason, err := c.deploymentRolloutReason(ctx, namespace, name, versionLabel, want)
		if err == nil && reason == "" {
			return nil
		}

		if err != nil {
			lastReason = err.Error()
		} else {
			lastReason = reason
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("deployment %s/%s did not roll out to %s=%q within %s: %s",
				namespace, name, versionLabel, want, c.rolloutTimeout(), lastReason)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.rolloutPollInterval()):
		}
	}
}

// deploymentRolloutReason fetches one Deployment and reports why it is
// not yet rolled out to versionLabel=want — empty when it is.
func (c *Cluster) deploymentRolloutReason(ctx context.Context, namespace, name, versionLabel, want string) (string, error) {
	out, err := c.runner().Output(ctx, c.kubectlArgs("get", "deployment", name, "-n", namespace, "-o", "json")...)
	if err != nil {
		return "", fmt.Errorf("get deployment %s/%s: %w", namespace, name, err)
	}

	var d deploymentState
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		return "", fmt.Errorf("parse deployment %s/%s: %w", namespace, name, err)
	}

	_, reason := d.rolledOut(versionLabel, want)

	return reason, nil
}

// WaitHTTPReady polls a URL until ANY HTTP response arrives — proves the
// service is listening without requiring a specific root handler — or
// the patience runs out.
func WaitHTTPReady(ctx context.Context, url string, patience time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(patience)

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
		if err != nil {
			return err
		}

		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()

			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("HTTP not ready at %s after %s: %w", url, patience, err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
