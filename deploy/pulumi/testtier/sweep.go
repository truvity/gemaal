package testtier

import (
	"fmt"
)

const defaultSweepDescription = "gemaal tenancy service: /test/ SSM state read-delete"

// DeploySweepIdentity creates the gemaal service identity: the tenancy
// housekeeping service sweeps orphaned test-tier state with it. Minimal on
// purpose, an allow-list in IAM form:
//
//   - SSM: read and delete under the /test/ root ONLY. Mirror-owned secret
//     paths are never janitored; this role simply cannot reach them.
//   - S3: the shared test bucket ONLY. List on the whole bucket (the sweep
//     reports unknown-shaped occupants), but Get and Delete confined to the
//     tenant key space: tenant prefixes are "<namespace>/<rel>/" and tenant
//     namespaces carry the TenantNamespaces patterns by construction, so IAM
//     structurally cannot delete anything that is not tenant-shaped.
//
// The identity is registered through register, the caller's pod-identity
// plumbing, which owns adoption of the association (and must never import
// it: a pre-existing unmanaged association should make the create fail
// loudly).
func DeploySweepIdentity(e Estate, register PodIdentityFunc) error {
	return register(PodIdentity{
		Name:           e.Cluster + "-gemaal",
		Namespace:      SweepNamespace,
		ServiceAccount: SweepServiceAccount,
		Description:    orDefault(e.Descriptions.SweepPolicy, defaultSweepDescription),
		PolicyDoc:      SweepPolicyDoc(e),
	})
}

// SweepPolicyDoc is the sweep identity's policy document.
func SweepPolicyDoc(e Estate) string {
	bucket := arn("s3", "", "", e.TestBucket)

	objects := make([]string, 0, len(e.TenantNamespaces))
	for _, ns := range e.TenantNamespaces {
		objects = append(objects, fmt.Sprintf("%s/%s", bucket, ns))
	}

	return mustJSON(map[string]any{
		polVersion: polDocDate,
		polStatement: []map[string]any{
			{
				polSid:    "GemaalTestStateReadDelete",
				polEffect: polAllow,
				polAction: []string{
					ssmGetParameter, ssmGetParameters, ssmGetParametersByPath,
					"ssm:DeleteParameter", "ssm:DeleteParameters",
				},
				polResource: arn("ssm", e.Region, e.AccountID, "parameter/test/*"),
			},
			{
				polSid:      "GemaalSecureStringDecrypt",
				polEffect:   polAllow,
				polAction:   kmsDecrypt,
				polResource: "*",
				polCondition: map[string]any{
					polStringEquals: map[string]any{
						condKMSViaService: fmt.Sprintf("ssm.%s.amazonaws.com", e.Region),
					},
				},
			},
			{
				polSid:      "GemaalTestBucketList",
				polEffect:   polAllow,
				polAction:   s3ListBucket,
				polResource: bucket,
			},
			{
				polSid:      "GemaalTestBucketTenantObjects",
				polEffect:   polAllow,
				polAction:   []string{s3GetObject, s3DeleteObject},
				polResource: objects,
			},
		},
	})
}
