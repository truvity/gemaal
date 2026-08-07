package engine

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// s3DeleteBatch is the DeleteObjects API maximum.
const s3DeleteBatch = 1000

// s3TestShapedBucket mirrors the config package's non-test-shaped
// tripwire: a bucket this client deletes in must carry a "test" path
// segment (e.g. "truvity-devel-test"). Config validation already refuses
// such a bucket at load; this is the last-possible-moment recheck, the
// same double rail DeleteParameters keeps for the /test/ SSM root.
var s3TestShapedBucket = regexp.MustCompile(`(^|-)test(-|$)`)

// S3API is the slice of the S3 service the client uses — what
// *s3.Client implements and a test stub fakes. The paginator half is
// the SDK's own interface so s3.NewListObjectsV2Paginator accepts it
// directly.
type S3API interface {
	s3.ListObjectsV2APIClient
	DeleteObjects(ctx context.Context, params *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// AWSS3Client is the S3Client backed by aws-sdk-go-v2, sweeping tenant
// prefixes in the shared test bucket (docs/architecture on the gitops
// side: layer-1, bucket "truvity-<cluster>-test").
type AWSS3Client struct {
	API S3API
}

// NewAWSS3Client builds an S3 client with the default credential chain
// (in-cluster, EKS Pod Identity supplies the credentials). Region should
// be the config's resolved region; empty falls through to the SDK's own
// environment resolution as a last resort.
func NewAWSS3Client(ctx context.Context, region string) (*AWSS3Client, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config for s3: %w", err)
	}

	return &AWSS3Client{API: s3.NewFromConfig(cfg)}, nil
}

// ListObjects streams every object in the bucket to visit. The whole
// bucket, deliberately: the sweep must SEE everything to report
// unknown-tenant-shape occupants, even though it only ever deletes
// tenant prefixes.
func (c *AWSS3Client) ListObjects(ctx context.Context, bucket string, visit func(Object) error) error {
	pager := s3.NewListObjectsV2Paginator(c.API, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
	})

	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list s3 objects in %s: %w", bucket, err)
		}

		for _, obj := range page.Contents {
			if obj.Key == nil || obj.LastModified == nil {
				continue
			}

			if err := visit(Object{Key: *obj.Key, LastModified: *obj.LastModified}); err != nil {
				return err
			}
		}
	}

	return nil
}

// DeletePrefix removes every object under one tenant prefix and returns
// how many went. Both halves of the address are re-checked at this last
// moment: the bucket must be test-shaped and the prefix must be a
// "<ns>/<rel>/" tenant container — never a bare top-level prefix, never
// a deeper path.
func (c *AWSS3Client) DeletePrefix(ctx context.Context, bucket, prefix string) (int, error) {
	if !s3TestShapedBucket.MatchString(bucket) {
		return 0, fmt.Errorf("%w: bucket %q is not test-shaped", ErrOutOfReach, bucket)
	}

	if !tenantContainerPrefix(prefix) {
		return 0, fmt.Errorf("%w: %q is not a tenant prefix (<ns>/<rel>/)", ErrOutOfReach, prefix)
	}

	// List fully, then delete in batches: tenant prefixes are small by
	// construction (test junk), and a stable key set beats paginating a
	// listing that mutates under its own paginator.
	var keys []string

	pager := s3.NewListObjectsV2Paginator(c.API, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})

	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return 0, fmt.Errorf("list s3://%s/%s: %w", bucket, prefix, err)
		}

		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}

			keys = append(keys, *obj.Key)
		}
	}

	deleted := 0

	for chunk := range slicesChunk(keys, s3DeleteBatch) {
		identifiers := make([]s3types.ObjectIdentifier, 0, len(chunk))
		for _, key := range chunk {
			identifiers = append(identifiers, s3types.ObjectIdentifier{Key: aws.String(key)})
		}

		out, err := c.API.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(bucket),
			Delete: &s3types.Delete{
				Objects: identifiers,
				// Quiet: only failures come back — Errors below is the
				// complete failure report.
				Quiet: aws.Bool(true),
			},
		})
		if err != nil {
			return deleted, fmt.Errorf("delete s3://%s/%s: %w", bucket, prefix, err)
		}

		if len(out.Errors) > 0 {
			first := out.Errors[0]

			return deleted + len(chunk) - len(out.Errors), fmt.Errorf(
				"delete s3://%s/%s: %d objects failed (first: %s: %s)",
				bucket, prefix, len(out.Errors),
				aws.ToString(first.Key), aws.ToString(first.Message))
		}

		deleted += len(chunk)
	}

	return deleted, nil
}

// tenantContainerPrefix reports whether prefix has exactly the
// "<ns>/<rel>/" shape: two non-empty segments and the trailing slash
// that makes it a container, never a record.
func tenantContainerPrefix(prefix string) bool {
	parts := strings.Split(prefix, "/")

	return len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] == ""
}
