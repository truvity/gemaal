package engine

import (
	"context"
	"time"
)

type (
	// Namespace is one tier-labeled namespace: the only kind the engine
	// ever sees. Selection happens by LABEL at the API server, so a
	// namespace without the tier label (gemaal-system, platform
	// namespaces) does not exist as far as the engine is concerned.
	Namespace struct {
		Name string
		Tier string
	}

	// Release is one Helm release as the engine needs it. Age comes from
	// Updated (helm's last-deployed time).
	Release struct {
		Namespace string
		Name      string
		Updated   time.Time
		Status    string

		// UpdatedProblem records an "updated" timestamp that refused to
		// parse; Updated is zero then. The age is the only thing standing
		// between a release and a TTL deletion, so an unreadable age
		// HOLDS the tenant — never "assume it is old" — and must not
		// stall planning for the rest of the estate.
		UpdatedProblem string
	}

	// ReleaseSecret is the newest release Secret of one release — the
	// resource the ledger labels ride on (helm >= 3.13 --labels).
	ReleaseSecret struct {
		Namespace string
		// Name is the Secret's own name (sh.helm.release.v1.<rel>.v<n>).
		Name string
		// Release is the release the Secret belongs to (its "name" label).
		Release string
		// Version is the release revision (its "version" label).
		Version int
		// Labels is the Secret's full label map, ledger labels included.
		Labels map[string]string
	}

	// Object is one S3 object in a test bucket.
	Object struct {
		Key          string
		LastModified time.Time
	}

	// Parameter is one SSM parameter under a /test/ root.
	Parameter struct {
		Name         string
		LastModified time.Time
	}

	// Lease is the sliver of a coordination.k8s.io Lease the engine
	// reads: whether a harness claim currently protects a tenant. A
	// zero Lease means absent.
	Lease struct {
		Holder          string
		RenewTime       time.Time
		DurationSeconds int
	}

	// KubeClient is the namespace selector and the ledger's storage.
	KubeClient interface {
		// ListTierNamespaces lists the namespaces whose tier label carries
		// one of the given values — selection by label, never by name.
		ListTierNamespaces(ctx context.Context, label string, tiers []string) ([]Namespace, error)

		// Lease reads one coordination.k8s.io Lease; absent yields the
		// zero Lease and no error (absence is an answer, not a failure).
		Lease(ctx context.Context, namespace, name string) (Lease, error)

		// ReleaseSecrets returns, per release in the namespace, the
		// NEWEST helm release Secret with its labels.
		ReleaseSecrets(ctx context.Context, namespace string) ([]ReleaseSecret, error)

		// LabelSecret sets labels on one Secret (kubectl label
		// --overwrite): how keep-until is written. Chosen over a helm
		// upgrade round-trip as the mechanically simplest write — see
		// docs/design.md ("Checkout, Extend and the keep-until write").
		LabelSecret(ctx context.Context, namespace, secret string, labels map[string]string) error
	}

	// HelmClient is the release truth. Listing is per namespace and
	// deliberately not "-A": a cluster-wide list that silently returns
	// fewer results under restricted RBAC would make live tenants look
	// dead, which is the one mistake this service must not make.
	HelmClient interface {
		ListReleases(ctx context.Context, namespace string) ([]Release, error)
		Uninstall(ctx context.Context, namespace, release string) error
	}

	// S3Client enumerates and deletes inside ONE bucket. The bucket is a
	// parameter on every call so no client ever holds a default target.
	S3Client interface {
		ListObjects(ctx context.Context, bucket string, visit func(Object) error) error
		DeletePrefix(ctx context.Context, bucket, prefix string) (int, error)
	}

	// SSMClient enumerates and deletes under ONE root.
	SSMClient interface {
		ListParameters(ctx context.Context, root string, visit func(Parameter) error) error
		DeleteParameters(ctx context.Context, names []string) error
	}
)
