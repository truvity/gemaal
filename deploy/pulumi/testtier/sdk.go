package testtier

import (
	"fmt"
	"log/slog"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/kms"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	defaultKMSKeyDescription = "SDK test fixture: disposable, test tier only"
	defaultAssumeDescription = "SDK assume-role test target: grants nothing by construction"
)

// DeploySDKTestKey mints the KMS key the SDK suites run against.
//
// Pulumi rather than a controller-managed key, unlike the test bucket: the
// grant needs the key's exact identity, and a Pulumi-created key hands it
// over directly. The deletion window is the 7-day floor. This key protects
// nothing durable; a fat-fingered destroy should not park a scheduled
// deletion for a month.
//
// Create it BEFORE anything that grants against its alias through a policy
// string: Pulumi cannot infer that dependency, and a grant on an alias that
// does not exist resolves to nothing.
func DeploySDKTestKey(e Estate) error {
	name := e.Cluster + "-sdk-test"

	key, err := kms.NewKey(e.Ctx, name, &kms.KeyArgs{
		Description:          pulumi.String(orDefault(e.Descriptions.KMSKey, defaultKMSKeyDescription)),
		DeletionWindowInDays: pulumi.Int(7),
		EnableKeyRotation:    pulumi.Bool(false),
	}, e.opts("key", name)...)
	if err != nil {
		return fmt.Errorf("sdk test kms key: %w", err)
	}

	// The alias is the stable handle. Tests name it rather than the key id,
	// so a key replacement does not rewrite the repositories that use it.
	if _, err := kms.NewAlias(e.Ctx, name, &kms.AliasArgs{
		Name:        pulumi.String(SDKTestAlias(e.Cluster)),
		TargetKeyId: key.KeyId,
	}, e.opts("alias", name)...); err != nil {
		return fmt.Errorf("sdk test kms alias: %w", err)
	}

	e.info("SDK test KMS key deployed", slog.String("alias", SDKTestAlias(e.Cluster)))

	return nil
}

// DeploySDKAssumeTarget mints a role whose ONLY purpose is to be assumed, so
// a suite testing a library's assume-role feature keeps testing it.
//
// It grants nothing. That is the point: the feature is exercised by the hop
// succeeding and the caller identity changing, not by anything the session
// can then do. A target with permissions would be a standing escalation path
// for zero extra coverage. With no principals nothing is created.
//
// The principals are the federated roles by their ARN outputs, not by names
// spelled here: IAM refuses a trust policy naming a role that does not exist
// yet, so the trust waits for the roles, and a renamed role moves into this
// trust by itself. The caller attaches SDKTestStatements to those roles; its
// SidSDKAssume statement is the other half of the hop.
func DeploySDKAssumeTarget(e Estate, principals []*iam.Role) error {
	name := e.Cluster + "-sdk-test-assume"

	if len(principals) == 0 {
		e.info("no federated roles; skipping SDK assume-role target", slog.String("cluster", e.Cluster))

		return nil
	}

	arns := make(pulumi.StringArray, 0, len(principals))
	for _, role := range principals {
		arns = append(arns, role.Arn)
	}

	trust := arns.ToStringArrayOutput().ApplyT(func(roleARNs []string) string {
		return mustJSON(map[string]any{
			polVersion: polDocDate,
			polStatement: []map[string]any{{
				polEffect: polAllow,
				// The roles, not the federated provider: this is a second
				// hop FROM an already-federated session, so the principal
				// is the role and the action is sts:AssumeRole rather than
				// the web-identity variant.
				polPrincipal: map[string]any{"AWS": roleARNs},
				polAction:    []string{stsAssumeRole},
			}},
		})
	}).(pulumi.StringOutput)

	policyDoc := mustJSON(map[string]any{
		polVersion: polDocDate,
		polStatement: []map[string]any{{
			// Deny everything, explicitly. An empty policy would work today
			// and silently inherit whatever a future attach adds; this
			// cannot.
			polSid:      "SDKAssumeTargetGrantsNothing",
			polEffect:   polDeny,
			polAction:   "*",
			polResource: "*",
		}},
	})

	policy, err := e.Kit.Policy(name, orDefault(e.Descriptions.AssumeTarget, defaultAssumeDescription), policyDoc)
	if err != nil {
		return err
	}

	if _, err := e.Kit.Role(name, trust, policy); err != nil {
		return err
	}

	e.info("SDK assume-role target deployed", slog.String("role", name))

	return nil
}

// SDKTestStatements are the grants the federated roles carry so the SDK
// suites run as either identity family (person or CI job).
//
// The KMS statement scopes by ALIAS rather than key ARN, via the
// kms:ResourceAliases condition key: a key ARN contains a generated id, so it
// is a Pulumi Output, and these documents are plain maps. The alias condition
// is exact (it resolves to the single key the alias points at) and survives a
// key replacement without a policy edit.
func SDKTestStatements(e Estate) []map[string]any {
	bucket := arn("s3", "", "", e.TestBucket)

	return []map[string]any{
		{
			// A client's ListBuckets is s3:ListAllMyBuckets, a different
			// action from ListBucket below, and one AWS does not let you
			// scope to a resource: account-wide or absent. It enumerates
			// bucket NAMES only. Acceptable for a dev loop; not to be copied
			// to a tier where the bucket list itself is worth keeping quiet.
			polSid:      SidSDKListAllBuckets,
			polEffect:   polAllow,
			polAction:   "s3:ListAllMyBuckets",
			polResource: "*",
		},
		{
			// Read AND write: these tests round-trip an object inside a
			// single test as a single identity. Delete included, unlike the
			// tenant path: nothing sweeps this prefix, so a suite that
			// cannot clean up leaves litter in a bucket with no expiry rule.
			polSid:      "SDKTestObjects",
			polEffect:   polAllow,
			polAction:   []string{s3GetObject, s3PutObject, s3DeleteObject},
			polResource: fmt.Sprintf("%s/%s/*", bucket, sdkTestPrefix),
		},
		{
			// Handler and storage tests write blobs under other prefixes.
			// Same disposable-data argument: the bucket is test-only, and
			// these roles cannot reach any other bucket.
			polSid:      "SDKTestAPIObjects",
			polEffect:   polAllow,
			polAction:   []string{s3GetObject, s3PutObject, s3DeleteObject},
			polResource: bucket + "/*",
		},
		{
			// UNCONDITIONED, deliberately. This began as ListBucket confined
			// by an s3:prefix condition, as the tenant path does it, and
			// that broke HeadBucket: HeadBucket maps to s3:ListBucket and
			// sends NO prefix, so the condition can never match.
			//
			// What this widens, exactly: these roles can ENUMERATE every key
			// in the test bucket, tenant prefixes included. Object contents
			// are open too, on purpose: SDKTestAPIObjects above grants
			// Get/Put/Delete on the whole bucket, because it is a test
			// bucket shared by every employee and every CI job, holding
			// disposable data only. Revisit if the shared bucket ever holds
			// anything whose key NAMES matter.
			//
			// GetBucketLocation beside it: bucket metadata, no key names or
			// contents, and an SDK asks for it before it talks to a bucket
			// in a region it was not told.
			polSid:      SidSDKListBucket,
			polEffect:   polAllow,
			polAction:   []string{s3ListBucket, s3GetBucketLocation},
			polResource: bucket,
		},
		{
			// The other half of the assume hop: the target's trust admits
			// these roles, and this lets them ask. Both are required, and
			// only this one lives in the caller.
			polSid:      SidSDKAssume,
			polEffect:   polAllow,
			polAction:   stsAssumeRole,
			polResource: arn("iam", "", e.AccountID, "role/"+e.Cluster+"-sdk-test-assume"),
		},
		{
			// ListKeys, like ListAllMyBuckets, cannot be scoped to a
			// resource. It returns key IDs and ARNs: no key material, no
			// metadata, and no ability to use any key the SDKTestKMS
			// statement below does not already allow.
			polSid:      SidSDKListKeys,
			polEffect:   polAllow,
			polAction:   "kms:ListKeys",
			polResource: "*",
		},
		{
			// No key management: these identities USE the key, they do not
			// administer it. Creating, disabling, scheduling deletion or
			// rewriting its policy stays with the deploy credential.
			//
			// The pair verb is what a key-pair generator actually calls: the
			// key material is generated inside KMS and only the wrapped
			// private half comes back. The symmetric GenerateDataKey stays
			// because other suites use it; the two are different
			// operations, not alternatives. Sign and Verify are not granted:
			// they work only on an asymmetric SIGN_VERIFY key, and this one
			// is symmetric.
			polSid:    "SDKTestKMS",
			polEffect: polAllow,
			polAction: []string{
				kmsEncrypt, kmsDecrypt, kmsGenerateDataKey,
				kmsGenerateDataKeyPairWithoutPlaintext, kmsDescribeKey,
				kmsGenerateDataKeyPair, kmsGenerateDataKeyWithoutPlaintext,
			},
			// Every key in the account is in scope for the ARN, and the
			// condition is what narrows it to one. Both halves matter: drop
			// the condition and this grants the whole account.
			polResource: arn("kms", e.Region, e.AccountID, "key/*"),
			polCondition: map[string]any{
				polForAnyValueStringEquals: map[string]any{
					condKMSResourceAliases: []string{SDKTestAlias(e.Cluster)},
				},
			},
		},
		{
			// A key-pair generator seeds from KMS randomness, so the
			// identity that generates a key needs this too. It takes no
			// resource, so it cannot carry the alias condition above and
			// cannot live in that statement: a resourceless call never
			// matches a ResourceAliases condition, which is why granting it
			// there would read as allowed and behave as denied. It returns
			// random bytes and nothing else.
			//
			// Symmetric with the tenant KMS role by design: both identities
			// that share this key hold the same use-only verb set, so a key
			// operation succeeding in-process and failing in a pod (or the
			// reverse) is not a thing that can happen.
			polSid:      "SDKTestKMSGenerateRandom",
			polEffect:   polAllow,
			polAction:   kmsGenerateRandom,
			polResource: "*",
		},
	}
}
