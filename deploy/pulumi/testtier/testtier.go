package testtier

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/k8s/pkg/aws/iamkit"
)

type (
	// Estate is the account, cluster and conventions the fixtures live in.
	Estate struct {
		// Ctx and Provider are the Pulumi context and the AWS provider of
		// the account and region. Required.
		Ctx      *pulumi.Context
		Provider pulumi.ProviderResource
		Logger   *slog.Logger

		// Kit creates the roles and policies that carry no import logic (the
		// assume target). Required by DeploySDKAssumeTarget only.
		Kit iamkit.Kit

		// Cluster is the EKS cluster the Pod Identity trust is pinned to.
		// Region and AccountID address every ARN. Required.
		Cluster   string
		Region    string
		AccountID string

		// BoundaryARN is the permissions boundary of the tenant roles. Empty
		// sets none.
		BoundaryARN string

		// TestBucket is the shared test bucket's name. Required.
		TestBucket string

		// TenantNamespaces are the namespace patterns (StringLike) of the
		// test tier, such as "emp-*" and "ci-*": the tenant trust admits
		// them, and the sweep may delete only under them. Required.
		TenantNamespaces []string

		// Prefix starts the logical Pulumi name of every tenant resource
		// ("<Prefix><role>/policy", ".../role", ".../attachment").
		Prefix string

		// PolicyExists and RoleExists report whether the object is live, for
		// the check-then-import of the tenant roles. Nil means "create
		// fresh". The policy is addressed by ARN, the role by name.
		PolicyExists func(arn string) bool
		RoleExists   func(name string) bool

		// Options returns extra options for the resource of the given kind
		// ("policy", "role", "attachment", "key", "alias") and logical name,
		// appended after the library's own. It is the hook an estate uses to
		// keep the aliases and URNs its resources already have.
		Options func(kind, logicalName string) []pulumi.ResourceOption

		// Descriptions are the immutable descriptions of the IAM policies and
		// the KMS key. A zero field takes the generic default.
		Descriptions Descriptions
	}

	// Descriptions are create-only: changing one replaces the object, so an
	// estate that adopted these resources passes the text they were created
	// with.
	Descriptions struct {
		KMSKey          string
		AssumeTarget    string
		TenantS3Policy  string
		TenantKMSPolicy string
		SweepPolicy     string
	}

	// PodIdentity is one service identity the caller registers: a policy, a
	// role and the association binding it to exactly one ServiceAccount.
	PodIdentity struct {
		Name           string
		Namespace      string
		ServiceAccount string
		Description    string
		PolicyDoc      string
	}

	// PodIdentityFunc registers a PodIdentity. The sweep identity is created
	// through the caller's pod-identity plumbing, which owns adoption of the
	// association.
	PodIdentityFunc func(PodIdentity) error
)

const (
	partition = "aws"

	sdkTestPrefix = "sdk-tests"

	// Sids the tests fence in by name.
	SidSDKListAllBuckets = "SDKTestListAllBuckets"
	SidSDKListBucket     = "SDKTestListBucket"
	SidSDKListKeys       = "SDKTestListKeys"
	SidSDKAssume         = "SDKTestAssumeRole"
	SidTenantWrite       = "TenantTestBucketWrite"
	SidTenantList        = "TenantTestBucketListOwnPrefix"
	SidTenantKMSKeyOps   = "TenantTestKMSKeyOps"
	SidTenantKMSRandom   = "TenantTestKMSGenerateRandom"

	// SweepNamespace and SweepServiceAccount are where the gemaal chart
	// runs the service.
	SweepNamespace      = "gemaal-system"
	SweepServiceAccount = "gemaal"

	podsPrincipal = "pods.eks.amazonaws.com"

	// Policy-language keys and values (the vocabulary of iamkit).
	polVersion   = iamkit.Version
	polStatement = iamkit.Statement
	polSid       = iamkit.Sid
	polEffect    = iamkit.Effect
	polAllow     = iamkit.Allow
	polDeny      = iamkit.Deny
	polAction    = iamkit.Action
	polResource  = iamkit.Resource
	polPrincipal = iamkit.Principal
	polCondition = iamkit.Condition
	polService   = iamkit.Service
	polDocDate   = iamkit.DocVersion

	polStringEquals            = iamkit.StringEquals
	polStringLike              = iamkit.StringLike
	polArnEquals               = iamkit.ArnEquals
	polForAnyValueStringEquals = iamkit.ForAnyValueStringEquals

	stsAssumeRole = iamkit.AssumeRole
	stsTagSession = iamkit.TagSession

	s3ListBucket        = "s3:ListBucket"
	s3GetBucketLocation = "s3:GetBucketLocation"
	s3GetObject         = "s3:GetObject"
	s3PutObject         = "s3:PutObject"
	s3DeleteObject      = "s3:DeleteObject"
	s3GetObjectTagging  = "s3:GetObjectTagging"
	s3PutObjectTagging  = "s3:PutObjectTagging"

	ssmGetParameter        = "ssm:GetParameter"
	ssmGetParameters       = "ssm:GetParameters"
	ssmGetParametersByPath = "ssm:GetParametersByPath"

	kmsEncrypt                             = "kms:Encrypt"
	kmsDecrypt                             = "kms:Decrypt"
	kmsGenerateDataKey                     = "kms:GenerateDataKey"
	kmsDescribeKey                         = "kms:DescribeKey"
	kmsGenerateDataKeyPairWithoutPlaintext = "kms:GenerateDataKeyPairWithoutPlaintext"
	kmsGenerateRandom                      = "kms:GenerateRandom"
	kmsGenerateDataKeyPair                 = "kms:GenerateDataKeyPair"
	kmsGenerateDataKeyWithoutPlaintext     = "kms:GenerateDataKeyWithoutPlaintext"

	condKMSViaService      = "kms:ViaService"
	condKMSResourceAliases = "kms:ResourceAliases"
	condS3Prefix           = "s3:prefix"
	condSourceAccount      = "aws:SourceAccount"
	condSourceArn          = "aws:SourceArn"
	condReqTagNamespace    = "aws:RequestTag/kubernetes-namespace"
)

func mustJSON(v any) string { return iamkit.MustJSON(v) }

// arn builds an ARN at runtime from its parts.
func arn(service, region, account, resource string) string {
	return fmt.Sprintf("arn:%s:%s:%s:%s:%s", partition, service, region, account, resource)
}

// SDKTestAlias is the stable handle the suites name and the policies'
// condition matches. One definition, so the two cannot drift.
func SDKTestAlias(cluster string) string {
	return "alias/" + cluster + "-sdk-test"
}

func (e Estate) opts(kind, logical string, base ...pulumi.ResourceOption) []pulumi.ResourceOption {
	opts := append([]pulumi.ResourceOption{pulumi.Provider(e.Provider)}, base...)
	if e.Options != nil {
		opts = append(opts, e.Options(kind, logical)...)
	}

	return opts
}

func (e Estate) policyExists(policyARN string) bool {
	return e.PolicyExists != nil && e.PolicyExists(policyARN)
}

func (e Estate) roleExists(name string) bool {
	return e.RoleExists != nil && e.RoleExists(name)
}

func (e Estate) info(msg string, attrs ...any) {
	if e.Logger != nil {
		e.Logger.InfoContext(e.context(), msg, attrs...)
	}
}

func (e Estate) context() context.Context {
	if e.Ctx == nil {
		return context.Background()
	}

	return e.Ctx.Context()
}

func orDefault(s, def string) string {
	if s != "" {
		return s
	}

	return def
}
