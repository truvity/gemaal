package testtier

import (
	"fmt"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	tenantS3Suffix  = "-tenant-test-s3"
	tenantKMSSuffix = "-tenant-test-kms"

	defaultTenantS3Description  = "shared test-tier workload identity: writes under the caller's own namespace prefix of the test bucket"
	defaultTenantKMSDescription = "shared test-tier vault identity: use-only KMS on the shared SDK test key, alias-confined"

	// tenantNamespaceTag is the session tag EKS Pod Identity stamps from the
	// pod's own namespace, resolved at request time.
	tenantNamespaceTag = "${aws:PrincipalTag/kubernetes-namespace}"
)

// DeployTenantS3 creates the ONE shared workload identity every test install
// writes through: one role with wildcard trust for the test tier's tenant
// namespaces. It is the write half of the object-storage act; the sweep
// identity (DeploySweepIdentity) is the delete half, and the two share the
// test bucket.
//
// Two properties do the security work:
//
//   - TRUST is namespace-shaped, not namespace-exact. EKS Pod Identity
//     session-tags every AssumeRole with kubernetes-namespace, so a StringLike
//     on the tenant patterns admits the whole test tier through one role. A
//     pod in any other namespace is structurally unable to assume it.
//   - The POLICY closes over the SAME tag. Access is confined to
//     "{namespace}/" inside the bucket, so cross-tenant access is an IAM
//     impossibility rather than a naming convention. Isolation is per
//     NAMESPACE, not per release.
//
// Deliberately NO PodIdentityAssociation here. The associations are
// roster-shaped (one per tenant namespace, appearing and vanishing with the
// namespace itself) and EKS rejects a wildcard serviceAccount, so one
// association per tenant namespace is the floor; they live with the
// namespaces.
func DeployTenantS3(e Estate) error {
	return e.deployTenantRole(
		e.Cluster+tenantS3Suffix,
		orDefault(e.Descriptions.TenantS3Policy, defaultTenantS3Description),
		TenantS3PolicyDoc(e),
	)
}

// DeployTenantKMS creates the shared workload identity a DEPLOYED vault
// assumes to reach KMS in a test install: the KMS sibling of DeployTenantS3,
// a second standing role rather than a widening of the first, because object
// storage and key use are different pods with different jobs, EKS Pod
// Identity binds each ServiceAccount to exactly one role, and folding KMS
// into the S3 role would hand every storage pod the key for no reason.
//
// Same trust as the S3 role; the policy is alias-confined instead of
// prefix-confined (a KMS key has no namespace to confine to), narrowing the
// account-wide key resource to the single shared SDK test key.
func DeployTenantKMS(e Estate) error {
	return e.deployTenantRole(
		e.Cluster+tenantKMSSuffix,
		orDefault(e.Descriptions.TenantKMSPolicy, defaultTenantKMSDescription),
		TenantKMSPolicyDoc(e),
	)
}

// TenantS3PolicyDoc is the tenant object-storage role's policy document.
// Exported so tests assert against the SAME document the deploy renders.
func TenantS3PolicyDoc(e Estate) string {
	bucket := arn("s3", "", "", e.TestBucket)

	return mustJSON(map[string]any{
		polVersion: polDocDate,
		polStatement: []map[string]any{
			{
				// Read, write and delete inside the tenant's own prefix, and
				// tagging: a blob lifecycle MOVES an object (copy, then
				// delete the original) and stamps a status tag on both, so
				// Get+Put alone let the copy succeed and the delete fail.
				// Confinement is unchanged: every action stays inside the
				// caller's namespace prefix, so a tenant can only move and
				// tag what it wrote.
				//
				// No multipart actions: the writer issues PutObject only, and
				// abandoned uploads are collected by the bucket's own
				// AbortIncompleteMultipartUpload lifecycle rule, which S3
				// executes itself.
				polSid:    SidTenantWrite,
				polEffect: polAllow,
				polAction: []string{
					s3PutObject, s3GetObject, s3DeleteObject,
					s3GetObjectTagging, s3PutObjectTagging,
				},
				polResource: fmt.Sprintf("%s/%s/*", bucket, tenantNamespaceTag),
			},
			{
				// Listing is bucket-scoped in IAM (the prefix is a request
				// parameter, not part of the ARN), so the tag condition has
				// to carry the confinement here.
				polSid:      SidTenantList,
				polEffect:   polAllow,
				polAction:   s3ListBucket,
				polResource: bucket,
				polCondition: map[string]any{
					polStringLike: map[string]any{
						condS3Prefix: []string{tenantNamespaceTag + "/*"},
					},
				},
			},
		},
	})
}

// TenantKMSPolicyDoc is the tenant key-use role's policy document. Exported
// so tests assert against the SAME document the deploy renders.
func TenantKMSPolicyDoc(e Estate) string {
	return mustJSON(map[string]any{
		polVersion: polDocDate,
		polStatement: []map[string]any{
			{
				// The vault USES the key: it envelope-encrypts private key
				// material (Encrypt/Decrypt), generates asymmetric pairs
				// without ever seeing the private half in plaintext, and
				// reads key metadata. It never administers the key, so a test
				// cannot damage the shared fixture.
				//
				// Scoped by ALIAS via kms:ResourceAliases, not the key ARN
				// (which carries a generated id, a Pulumi Output). It names
				// the SAME key the SDK suites use, so an in-process key and
				// the deployed vault share one key.
				polSid:    SidTenantKMSKeyOps,
				polEffect: polAllow,
				polAction: []string{
					kmsEncrypt, kmsDecrypt,
					kmsGenerateDataKeyPairWithoutPlaintext, kmsDescribeKey,
				},
				polResource: arn("kms", e.Region, e.AccountID, "key/*"),
				polCondition: map[string]any{
					polForAnyValueStringEquals: map[string]any{
						condKMSResourceAliases: []string{SDKTestAlias(e.Cluster)},
					},
				},
			},
			{
				// GenerateRandom takes no resource, so it cannot carry the
				// alias condition and is granted account-wide. It returns
				// random bytes and nothing else.
				polSid:      SidTenantKMSRandom,
				polEffect:   polAllow,
				polAction:   kmsGenerateRandom,
				polResource: "*",
			},
		},
	})
}

// tenantTrust is the trust document of both tenant roles: the Pod Identity
// service principal, pinned to this account and cluster, admitting only the
// tenant namespaces through the session tag.
func (e Estate) tenantTrust() string {
	return mustJSON(map[string]any{
		polVersion: polDocDate,
		polStatement: []map[string]any{{
			polEffect:    polAllow,
			polPrincipal: map[string]any{polService: podsPrincipal},
			polAction:    []string{stsAssumeRole, stsTagSession},
			polCondition: map[string]any{
				polStringEquals: map[string]any{
					condSourceAccount: e.AccountID,
				},
				polArnEquals: map[string]any{
					condSourceArn: arn("eks", e.Region, e.AccountID, "cluster/"+e.Cluster),
				},
				// The tier gate.
				polStringLike: map[string]any{
					condReqTagNamespace: e.TenantNamespaces,
				},
			},
		}},
	})
}

// deployTenantRole registers the policy, role and attachment of one tenant
// identity, adopting whichever already exist (check-then-import).
func (e Estate) deployTenantRole(name, description, policyDoc string) error {
	policyARN := arn("iam", "", e.AccountID, "policy/"+name)

	var policyImport []pulumi.ResourceOption
	if e.policyExists(policyARN) {
		policyImport = append(policyImport, pulumi.Import(pulumi.ID(policyARN)))
	}

	// Descriptions are create-only and the check-then-import adoption refuses
	// replaces, so the text is frozen once an estate has created the policy.
	policy, err := iam.NewPolicy(e.Ctx, e.Prefix+name+"/policy", &iam.PolicyArgs{
		Name:        pulumi.String(name),
		Description: pulumi.String(description),
		Policy:      pulumi.String(policyDoc),
	}, e.opts("policy", e.Prefix+name+"/policy", policyImport...)...)
	if err != nil {
		return fmt.Errorf("policy: %w", err)
	}

	roleExists := e.roleExists(name)

	var roleImport, attachImport []pulumi.ResourceOption
	if roleExists {
		roleImport = append(roleImport, pulumi.Import(pulumi.ID(name)))
		attachImport = append(attachImport, pulumi.Import(pulumi.ID(name+"/"+policyARN)))
	}

	roleArgs := &iam.RoleArgs{
		Name:             pulumi.String(name),
		AssumeRolePolicy: pulumi.String(e.tenantTrust()),
	}
	// Set even when empty: the shape of the registered inputs is state.
	roleArgs.PermissionsBoundary = pulumi.String(e.BoundaryARN)

	role, err := iam.NewRole(e.Ctx, e.Prefix+name+"/role", roleArgs,
		e.opts("role", e.Prefix+name+"/role", roleImport...)...)
	if err != nil {
		return fmt.Errorf("role: %w", err)
	}

	if _, err := iam.NewRolePolicyAttachment(e.Ctx, e.Prefix+name+"/attachment", &iam.RolePolicyAttachmentArgs{
		Role:      role.Name,
		PolicyArn: policy.Arn,
	}, e.opts("attachment", e.Prefix+name+"/attachment", attachImport...)...); err != nil {
		return fmt.Errorf("attachment: %w", err)
	}

	return nil
}
