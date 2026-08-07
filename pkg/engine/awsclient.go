package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

const (
	// ssmDeleteBatch is the DeleteParameters API maximum.
	ssmDeleteBatch = 10

	// ssmTestRoot is the ONLY SSM tree the service may ever delete
	// under. The refusal is enforced at the last possible moment as well
	// as the first (config validation): "/secrets/**" has a different
	// writer and is never gemaal's.
	ssmTestRoot = "/test/"
)

// SSMAPI is the slice of the SSM service the client uses — what
// *ssm.Client implements and a test stub fakes. The paginator half is
// the SDK's own interface so ssm.NewGetParametersByPathPaginator
// accepts it directly.
type SSMAPI interface {
	ssm.GetParametersByPathAPIClient
	DeleteParameters(ctx context.Context, params *ssm.DeleteParametersInput, optFns ...func(*ssm.Options)) (*ssm.DeleteParametersOutput, error)
}

// AWSSSMClient is the SSMClient backed by aws-sdk-go-v2. The S3
// counterpart is AWSS3Client (awss3client.go), built the same way.
type AWSSSMClient struct {
	API SSMAPI
}

// NewAWSSSMClient builds an SSM client with the default credential
// chain (in-cluster, EKS Pod Identity supplies the credentials). Region
// should be the config's resolved region; empty falls through to the
// SDK's own environment resolution as a last resort.
func NewAWSSSMClient(ctx context.Context, region string) (*AWSSSMClient, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config for ssm: %w", err)
	}

	return &AWSSSMClient{API: ssm.NewFromConfig(cfg)}, nil
}

// ListParameters streams every parameter under root to visit.
func (c *AWSSSMClient) ListParameters(ctx context.Context, root string, visit func(Parameter) error) error {
	pager := ssm.NewGetParametersByPathPaginator(c.API, &ssm.GetParametersByPathInput{
		Path:      aws.String(root),
		Recursive: aws.Bool(true),
	})

	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list ssm parameters under %s: %w", root, err)
		}

		for _, param := range page.Parameters {
			if param.Name == nil || param.LastModifiedDate == nil {
				continue
			}

			if err := visit(Parameter{Name: *param.Name, LastModified: *param.LastModifiedDate}); err != nil {
				return err
			}
		}
	}

	return nil
}

// DeleteParameters removes parameters in batches. Every name is
// re-checked against the /test/ root.
func (c *AWSSSMClient) DeleteParameters(ctx context.Context, names []string) error {
	for _, name := range names {
		if !strings.HasPrefix(name, ssmTestRoot) {
			return fmt.Errorf("%w: parameter %q is not under %q", ErrOutOfReach, name, ssmTestRoot)
		}
	}

	for chunk := range slicesChunk(names, ssmDeleteBatch) {
		out, err := c.API.DeleteParameters(ctx, &ssm.DeleteParametersInput{Names: chunk})
		if err != nil {
			return fmt.Errorf("delete ssm parameters: %w", err)
		}

		if len(out.InvalidParameters) > 0 {
			return fmt.Errorf("delete ssm parameters: %d invalid (first: %s)",
				len(out.InvalidParameters), out.InvalidParameters[0])
		}
	}

	return nil
}

// slicesChunk yields fixed-size windows over s.
func slicesChunk[T any](s []T, size int) func(func([]T) bool) {
	return func(yield func([]T) bool) {
		for start := 0; start < len(s); start += size {
			end := min(start+size, len(s))

			if !yield(s[start:end]) {
				return
			}
		}
	}
}
