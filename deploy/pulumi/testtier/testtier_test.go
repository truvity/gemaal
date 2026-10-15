package testtier

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The documents are attached to live roles, so their bytes are pinned: the
// goldens were written from the code these documents were moved from, and a
// change to a single byte is a change to a deployed policy. "arn:PARTITION:"
// stands for the partition prefix, which the files keep out of the source.
func golden(t *testing.T, name string) string {
	t.Helper()

	raw, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)

	return strings.ReplaceAll(strings.TrimSpace(string(raw)), "arn:PARTITION:", "arn:"+partition+":")
}

func estate() Estate {
	return Estate{
		Cluster:          "alpha",
		Region:           "eu-central-1",
		AccountID:        "123456789012",
		TestBucket:       "example-test",
		TenantNamespaces: []string{"emp-*", "ci-*"},
	}
}

func TestSDKTestStatementsAreByteStable(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": SDKTestStatements(estate())})
	require.NoError(t, err)
	require.Equal(t, golden(t, "sdk-test-policy.json"), string(raw))
}

func TestTenantDocumentsAreByteStable(t *testing.T) {
	t.Parallel()

	e := estate()
	require.Equal(t, golden(t, "tenant-s3-policy.json"), TenantS3PolicyDoc(e))
	require.Equal(t, golden(t, "tenant-kms-policy.json"), TenantKMSPolicyDoc(e))
	require.Equal(t, golden(t, "tenant-trust.json"), e.tenantTrust())
}

func TestSweepPolicyIsByteStable(t *testing.T) {
	t.Parallel()

	require.Equal(t, golden(t, "sweep-policy.json"), SweepPolicyDoc(estate()))
}

func TestSweepIdentityIsRegisteredThroughTheCaller(t *testing.T) {
	t.Parallel()

	var got PodIdentity

	require.NoError(t, DeploySweepIdentity(estate(), func(p PodIdentity) error {
		got = p

		return nil
	}))
	require.Equal(t, "alpha-gemaal", got.Name)
	require.Equal(t, SweepNamespace, got.Namespace)
	require.Equal(t, SweepServiceAccount, got.ServiceAccount)
}

// The tenant grants close over the session tag; without it a tenant could
// reach another's prefix. The sweep may delete only tenant-shaped keys.
func TestTenantGrantsStayConfined(t *testing.T) {
	t.Parallel()

	e := estate()
	require.Contains(t, TenantS3PolicyDoc(e), "${aws:PrincipalTag/kubernetes-namespace}/*")
	require.Contains(t, TenantKMSPolicyDoc(e), SDKTestAlias("alpha"))
	require.Equal(t, "alias/alpha-sdk-test", SDKTestAlias("alpha"))

	sweep := SweepPolicyDoc(e)
	require.Contains(t, sweep, "example-test/emp-*")
	require.Contains(t, sweep, "example-test/ci-*")
	require.NotContains(t, sweep, "example-test/*")
}
