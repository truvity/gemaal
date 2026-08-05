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
