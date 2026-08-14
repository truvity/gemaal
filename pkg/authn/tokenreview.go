package authn

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// In-cluster service-account paths and environment, as Kubernetes mounts
// them.
const (
	saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // a path, not a credential
	saCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	envAPIHost  = "KUBERNETES_SERVICE_HOST"
	envAPIPort  = "KUBERNETES_SERVICE_PORT"
)

// ErrNotInCluster is returned when the in-cluster environment is absent.
var ErrNotInCluster = errors.New("authn: not running in a cluster (no KUBERNETES_SERVICE_HOST)")

// KubeTokenReviewer validates tokens with the TokenReview API, spoken
// directly over HTTP — the deliberate no-client-go choice this repo
// makes everywhere (the nats-auth-callout donor links client-go for the
// same call; here one POST does not justify the dependency tree).
type KubeTokenReviewer struct {
	// BaseURL is the API server, e.g. "https://10.96.0.1:443".
	BaseURL string

	// Token authenticates THIS service to the API server. When
	// TokenPath is set it is a fallback only (see Review).
	Token string

	// TokenPath, when non-empty, is re-read on every Review: the
	// kubelet rotates the projected service-account token in place,
	// and a token captured once at construction expires with its
	// first lifetime — observed in devel as every TokenReview
	// answering 401 from ~24h after pod start (2026-08-13 19:05Z)
	// until a restart. One small file read per authentication is the
	// no-client-go price of staying current.
	TokenPath string

	// Client is the HTTP client (carrying the cluster CA in-cluster).
	Client *http.Client

	// Audiences restricts what the reviewed token must be intended for;
	// empty means the API server's default.
	Audiences []string
}

// NewInClusterTokenReviewer builds a reviewer from the pod's own service
// account.
func NewInClusterTokenReviewer(audiences []string) (*KubeTokenReviewer, error) {
	host, port := os.Getenv(envAPIHost), os.Getenv(envAPIPort)
	if host == "" || port == "" {
		return nil, ErrNotInCluster
	}

	return newTokenReviewer(host, port, saTokenPath, saCAPath, audiences)
}

// newTokenReviewer is the file-reading half, split so the assembly is
// testable without a pod's mounted service account.
func newTokenReviewer(host, port, tokenPath, caPath string, audiences []string) (*KubeTokenReviewer, error) {
	token, err := os.ReadFile(tokenPath) //nolint:gosec // the path is a compile-time constant (or a test's)
	if err != nil {
		return nil, fmt.Errorf("read service-account token: %w", err)
	}

	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read cluster CA: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("authn: cluster CA bundle holds no certificates")
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}

	return &KubeTokenReviewer{
		BaseURL:   "https://" + host + ":" + port,
		Token:     string(bytes.TrimSpace(token)),
		TokenPath: tokenPath,
		Client:    client,
		Audiences: audiences,
	}, nil
}

// tokenReview is the authentication.k8s.io/v1 wire shape — exactly the
// fields this service reads and writes.
type tokenReview struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Spec       tokenReviewSpec `json:"spec"`
	Status     struct {
		Authenticated bool `json:"authenticated"`
		User          struct {
			Username string   `json:"username"`
			Groups   []string `json:"groups"`
		} `json:"user"`
		Error string `json:"error"`
	} `json:"status"`
}

type tokenReviewSpec struct {
	Token     string   `json:"token"`
	Audiences []string `json:"audiences,omitempty"`
}

// bearer returns the freshest service credential: the projected token
// file when TokenPath is set (the kubelet rotates it in place), else
// the static Token. A read failure falls back to the static token so a
// transient filesystem error degrades to the old behavior instead of
// failing every authentication outright.
func (r *KubeTokenReviewer) bearer() string {
	if r.TokenPath == "" {
		return r.Token
	}

	token, err := os.ReadFile(r.TokenPath) //nolint:gosec // constructor-controlled path
	if err != nil {
		return r.Token
	}

	return string(bytes.TrimSpace(token))
}

// Review implements TokenReviewer.
func (r *KubeTokenReviewer) Review(ctx context.Context, token string) (Identity, bool, error) {
	body, err := json.Marshal(tokenReview{
		APIVersion: "authentication.k8s.io/v1",
		Kind:       "TokenReview",
		Spec:       tokenReviewSpec{Token: token, Audiences: r.Audiences},
	})
	if err != nil {
		return Identity{}, false, fmt.Errorf("marshal tokenreview: %w", err)
	}

	url := r.BaseURL + "/apis/authentication.k8s.io/v1/tokenreviews"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Identity{}, false, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.bearer())

	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return Identity{}, false, fmt.Errorf("post tokenreview: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return Identity{}, false, fmt.Errorf("tokenreview: unexpected status %s", resp.Status)
	}

	var review tokenReview
	if err := json.NewDecoder(resp.Body).Decode(&review); err != nil {
		return Identity{}, false, fmt.Errorf("decode tokenreview response: %w", err)
	}

	if !review.Status.Authenticated {
		return Identity{}, false, nil
	}

	return Identity{
		Subject: review.Status.User.Username,
		Groups:  review.Status.User.Groups,
		Method:  MethodTokenReview,
	}, true, nil
}
