package harness

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTierForNamespace(t *testing.T) {
	tests := []struct {
		namespace string
		want      string
	}{
		{"emp-jdoe", TierEmployee},
		{"ci-myorg-myapp", TierCI},
		{"ci-legacy", TierCI},
		{"kube-system", TierUnknown},
		{"employee-jdoe", TierUnknown}, // the retired prefix is nobody's tier
		{"emp", TierUnknown},           // the prefix includes the dash
		{"", TierUnknown},
	}

	for _, tt := range tests {
		t.Run("ns="+tt.namespace, func(t *testing.T) {
			assert.Equal(t, tt.want, TierForNamespace(tt.namespace))
		})
	}
}

func TestDetectTier(t *testing.T) {
	tests := []struct {
		name        string
		env         string
		kubecontext string
		want        string
	}{
		{"no env, no kind context: shared tier", "", "devel@oidc", ""},
		{"no env, no context at all: shared tier", "", "", ""},
		{"kind- context with no env override", "", "kind-mycluster", TierKind},
		{"env wins over a non-kind context", TierKind, "devel@oidc", TierKind},
		{"env wins over an absent context", TierKind, "", TierKind},
		{"env carries any value, not just kind", "custom-tier", "devel@oidc", "custom-tier"},
		{"a context merely containing kind- is not a prefix match", "", "my-kind-cluster", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvTier, tt.env)
			assert.Equal(t, tt.want, DetectTier(tt.kubecontext))
		})
	}
}

func TestClusterTier(t *testing.T) {
	t.Run("explicit Tier wins over context detection", func(t *testing.T) {
		t.Setenv(EnvTier, "")
		c := &Cluster{Kubecontext: "kind-mycluster", Tier: TierEmployee}
		assert.Equal(t, TierEmployee, c.tier())
	})

	t.Run("empty Tier auto-detects from Kubecontext", func(t *testing.T) {
		t.Setenv(EnvTier, "")
		c := &Cluster{Kubecontext: "kind-mycluster"}
		assert.Equal(t, TierKind, c.tier())
	})

	t.Run("an ordinary context stays the shared tier, unchanged", func(t *testing.T) {
		t.Setenv(EnvTier, "")
		c := &Cluster{Kubecontext: "devel@oidc"}
		assert.Equal(t, "", c.tier())
	})
}
