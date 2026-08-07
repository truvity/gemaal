package engine_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/gemaal/pkg/engine"
)

// stubS3 scripts the two S3 calls the client makes and records every
// request for shaping assertions. No live AWS anywhere near the tests.
type stubS3 struct {
	pages   []*s3.ListObjectsV2Output
	listErr error

	listCalls   []*s3.ListObjectsV2Input
	deleteCalls []*s3.DeleteObjectsInput

	deleteOut *s3.DeleteObjectsOutput
	deleteErr error
}

var _ engine.S3API = (*stubS3)(nil)

func (s *stubS3) ListObjectsV2(
	_ context.Context, params *s3.ListObjectsV2Input, _ ...func(*s3.Options),
) (*s3.ListObjectsV2Output, error) {
	s.listCalls = append(s.listCalls, params)

	if s.listErr != nil {
		return nil, s.listErr
	}

	page := len(s.listCalls) - 1
	if page >= len(s.pages) {
		panic("stubS3: paginator asked past the scripted pages")
	}

	return s.pages[page], nil
}

func (s *stubS3) DeleteObjects(
	_ context.Context, params *s3.DeleteObjectsInput, _ ...func(*s3.Options),
) (*s3.DeleteObjectsOutput, error) {
	s.deleteCalls = append(s.deleteCalls, params)

	if s.deleteErr != nil {
		return nil, s.deleteErr
	}

	if s.deleteOut != nil {
		return s.deleteOut, nil
	}

	return &s3.DeleteObjectsOutput{}, nil
}

// object builds one well-formed listing row.
func object(key string, modified time.Time) s3types.Object {
	return s3types.Object{Key: aws.String(key), LastModified: aws.Time(modified)}
}

func TestAWSS3ClientListShapesAndPaginates(t *testing.T) {
	t.Parallel()

	stub := &stubS3{pages: []*s3.ListObjectsV2Output{
		{
			Contents: []s3types.Object{
				object("emp-alice/demo/report.json", now.Add(-2*time.Hour)),
				object("emp-alice/demo/artifacts/log.txt", now.Add(-time.Hour)),
			},
			IsTruncated:           aws.Bool(true),
			NextContinuationToken: aws.String("page-2"),
		},
		{
			Contents: []s3types.Object{
				object("ci-truvity-bar/r7-a1/dump.bin", now.Add(-30*time.Minute)),
			},
		},
	}}
	client := &engine.AWSS3Client{API: stub}

	var seen []engine.Object

	err := client.ListObjects(context.Background(), "truvity-devel-test", func(o engine.Object) error {
		seen = append(seen, o)

		return nil
	})
	require.NoError(t, err)

	// Every page's rows arrive, in order, with both fields carried over.
	require.Len(t, seen, 3)
	assert.Equal(t, "emp-alice/demo/report.json", seen[0].Key)
	assert.Equal(t, now.Add(-2*time.Hour), seen[0].LastModified)
	assert.Equal(t, "ci-truvity-bar/r7-a1/dump.bin", seen[2].Key)

	// Request shaping: the bucket, NO prefix (the sweep must see
	// everything to report unknown-tenant-shape occupants), and the
	// continuation token threaded from the first response into the
	// second request.
	require.Len(t, stub.listCalls, 2)
	assert.Equal(t, "truvity-devel-test", aws.ToString(stub.listCalls[0].Bucket))
	assert.Nil(t, stub.listCalls[0].Prefix, "the full-bucket listing must not narrow to a prefix")
	assert.Nil(t, stub.listCalls[0].ContinuationToken)
	assert.Equal(t, "page-2", aws.ToString(stub.listCalls[1].ContinuationToken))
}

func TestAWSS3ClientListSkipsIncompleteRows(t *testing.T) {
	t.Parallel()

	stub := &stubS3{pages: []*s3.ListObjectsV2Output{{
		Contents: []s3types.Object{
			{Key: nil, LastModified: aws.Time(now)},
			{Key: aws.String("emp-alice/demo/undated"), LastModified: nil},
			object("emp-alice/demo/whole", now),
		},
	}}}
	client := &engine.AWSS3Client{API: stub}

	var seen []string

	err := client.ListObjects(context.Background(), "truvity-devel-test", func(o engine.Object) error {
		seen = append(seen, o.Key)

		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"emp-alice/demo/whole"}, seen)
}

func TestAWSS3ClientListVisitErrorStopsListing(t *testing.T) {
	t.Parallel()

	boom := errors.New("visit refused")
	stub := &stubS3{pages: []*s3.ListObjectsV2Output{
		{
			Contents:              []s3types.Object{object("emp-alice/demo/a", now)},
			IsTruncated:           aws.Bool(true),
			NextContinuationToken: aws.String("page-2"),
		},
		{Contents: []s3types.Object{object("emp-alice/demo/b", now)}},
	}}
	client := &engine.AWSS3Client{API: stub}

	err := client.ListObjects(context.Background(), "truvity-devel-test", func(engine.Object) error { return boom })
	require.ErrorIs(t, err, boom)
	assert.Len(t, stub.listCalls, 1, "a refusing visitor must stop the listing, not just skip the row")
}

func TestAWSS3ClientListAPIErrorNamesBucket(t *testing.T) {
	t.Parallel()

	stub := &stubS3{listErr: errors.New("AccessDenied")}
	client := &engine.AWSS3Client{API: stub}

	err := client.ListObjects(context.Background(), "truvity-devel-test", func(engine.Object) error { return nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "truvity-devel-test", "the error must name the bucket it was listing")
	assert.Contains(t, err.Error(), "AccessDenied")
}

func TestAWSS3ClientDeletePrefixListsThenBatches(t *testing.T) {
	t.Parallel()

	// 2500 keys: two full DeleteObjects batches of 1000 and one of 500.
	contents := make([]s3types.Object, 2500)
	for i := range contents {
		contents[i] = object(fmt.Sprintf("emp-alice/demo/obj-%04d", i), now)
	}

	stub := &stubS3{pages: []*s3.ListObjectsV2Output{{Contents: contents}}}
	client := &engine.AWSS3Client{API: stub}

	deleted, err := client.DeletePrefix(context.Background(), "truvity-devel-test", "emp-alice/demo/")
	require.NoError(t, err)
	assert.Equal(t, 2500, deleted)

	// The delete listing narrows to the tenant prefix.
	require.Len(t, stub.listCalls, 1)
	assert.Equal(t, "emp-alice/demo/", aws.ToString(stub.listCalls[0].Prefix))

	// The DeleteObjects API caps at 1000 objects per call; quiet mode so
	// the response carries failures only.
	require.Len(t, stub.deleteCalls, 3)
	assert.Len(t, stub.deleteCalls[0].Delete.Objects, 1000)
	assert.Len(t, stub.deleteCalls[1].Delete.Objects, 1000)
	assert.Len(t, stub.deleteCalls[2].Delete.Objects, 500)
	assert.True(t, aws.ToBool(stub.deleteCalls[0].Delete.Quiet))

	var sent []string
	for _, call := range stub.deleteCalls {
		for _, id := range call.Delete.Objects {
			sent = append(sent, aws.ToString(id.Key))
		}
	}

	require.Len(t, sent, 2500)
	assert.Equal(t, "emp-alice/demo/obj-0000", sent[0])
	assert.Equal(t, "emp-alice/demo/obj-2499", sent[2499], "batching must preserve every key in order")
}

func TestAWSS3ClientDeletePrefixEmptyPrefixDeletesNothing(t *testing.T) {
	t.Parallel()

	stub := &stubS3{pages: []*s3.ListObjectsV2Output{{}}}
	client := &engine.AWSS3Client{API: stub}

	deleted, err := client.DeletePrefix(context.Background(), "truvity-devel-test", "emp-alice/demo/")
	require.NoError(t, err)
	assert.Zero(t, deleted)
	assert.Empty(t, stub.deleteCalls, "an already-empty prefix must not produce a DeleteObjects call")
}

func TestAWSS3ClientDeletePrefixRefusesNonTestShapedBucket(t *testing.T) {
	t.Parallel()

	stub := &stubS3{}
	client := &engine.AWSS3Client{API: stub}

	_, err := client.DeletePrefix(context.Background(), "customer-data", "emp-alice/demo/")
	require.ErrorIs(t, err, engine.ErrOutOfReach)
	assert.Empty(t, stub.listCalls, "a non-test-shaped bucket must refuse before any API request")
	assert.Empty(t, stub.deleteCalls)
}

func TestAWSS3ClientDeletePrefixRefusesNonTenantPrefixes(t *testing.T) {
	t.Parallel()

	for _, prefix := range []string{
		"",                    // the whole bucket
		"emp-alice/",          // a bare namespace, not a tenant
		"emp-alice/demo",      // a record, not a container
		"emp-alice/demo/sub/", // deeper than a tenant container
		"/emp-alice/demo/",    // S3 keys do not lead with a slash
	} {
		stub := &stubS3{}
		client := &engine.AWSS3Client{API: stub}

		_, err := client.DeletePrefix(context.Background(), "truvity-devel-test", prefix)
		require.ErrorIs(t, err, engine.ErrOutOfReach, "prefix %q", prefix)
		assert.Empty(t, stub.listCalls, "prefix %q must refuse before any API request", prefix)
	}
}

func TestAWSS3ClientDeletePrefixSurfacesPerObjectErrors(t *testing.T) {
	t.Parallel()

	stub := &stubS3{
		pages: []*s3.ListObjectsV2Output{{Contents: []s3types.Object{
			object("emp-alice/demo/a", now),
			object("emp-alice/demo/b", now),
		}}},
		deleteOut: &s3.DeleteObjectsOutput{Errors: []s3types.Error{{
			Key:     aws.String("emp-alice/demo/b"),
			Message: aws.String("AccessDenied"),
		}}},
	}
	client := &engine.AWSS3Client{API: stub}

	deleted, err := client.DeletePrefix(context.Background(), "truvity-devel-test", "emp-alice/demo/")
	require.Error(t, err)
	assert.Equal(t, 1, deleted, "the count must reflect what actually went")
	assert.Contains(t, err.Error(), "emp-alice/demo/b")
	assert.Contains(t, err.Error(), "AccessDenied")
}

func TestAWSS3ClientDeletePrefixAPIError(t *testing.T) {
	t.Parallel()

	boom := errors.New("ThrottlingException")
	stub := &stubS3{
		pages:     []*s3.ListObjectsV2Output{{Contents: []s3types.Object{object("emp-alice/demo/a", now)}}},
		deleteErr: boom,
	}
	client := &engine.AWSS3Client{API: stub}

	_, err := client.DeletePrefix(context.Background(), "truvity-devel-test", "emp-alice/demo/")
	require.ErrorIs(t, err, boom)
}
