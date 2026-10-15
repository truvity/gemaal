// Package testtier is the AWS side of a test tier: the shared S3 bucket and
// KMS key that tests run against, the namespace-confined workload identities
// that reach them, and the identity the gemaal tenancy service sweeps them
// with.
//
// Two paths reach the fixtures, and the difference is the point:
//
//   - The TENANT path is pod-identity shaped. Its trust admits the EKS Pod
//     Identity service principal for tenant namespaces only, and its policy
//     closes over ${aws:PrincipalTag/kubernetes-namespace}, which EKS stamps
//     from the pod's own namespace. Cross-tenant access is an IAM
//     impossibility rather than a naming convention. DeployTenantS3 and
//     DeployTenantKMS are its two identities (object storage and key use),
//     kept apart because the workloads that use them are.
//   - The SDK path is for test processes that are not pods in a tenant
//     namespace (a laptop, a CI runner). SDKTestStatements are the grants
//     the caller attaches to its federated roles, confined by a FIXED
//     prefix and the key's alias rather than a session tag, and
//     DeploySDKAssumeTarget is the role those suites assume to prove the
//     library's assume-role hop. Isolation inside the prefix is by
//     convention; do not copy this where the data matters.
//
// DeploySweepIdentity is the gemaal service's own identity: read and delete
// under the /test/ parameter root, list the bucket, and get and delete
// objects only in tenant-shaped key space, so IAM structurally cannot delete
// anything that is not tenant-shaped.
//
// Every particular is an input (Estate). Descriptions of IAM policies are
// immutable, so an estate that already runs these resources passes the
// descriptions it created them with (Descriptions), and the resource options
// hook (Estate.Options) lets it keep the aliases, parents and logical names
// its state already has.
package testtier
