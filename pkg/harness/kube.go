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

func (c *Cluster) rolloutTimeout() time.Duration {
	if c.RolloutTimeout > 0 {
		return c.RolloutTimeout
	}

	return DefaultRolloutTimeout
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

// ServiceURL renders "http://<clusterIP>:<port>" for a Service — reachable
// directly when the operator routes the service CIDR to the test network
// (e.g. a tailnet subnet route), which is what makes port-forwards
// unnecessary in this harness.
func (c *Cluster) ServiceURL(ctx context.Context, namespace, service string, port int) (string, error) {
	ip, err := c.ServiceClusterIP(ctx, namespace, service)
	if err != nil {
		return "", err
	}

	return "http://" + ip + ":" + strconv.Itoa(port), nil
}

// ReleaseDeployments lists the Deployments owned by any of the given
// releases (the meta.helm.sh/release-name annotation helm stamps).
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
		if owned[list.Items[i].Metadata.Annotations["meta.helm.sh/release-name"]] {
			names = append(names, list.Items[i].Metadata.Name)
		}
	}

	return names, nil
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
