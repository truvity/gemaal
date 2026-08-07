package engine_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/gemaal/pkg/engine"
)

// stubSSM scripts the two SSM calls the client makes and records every
// request for shaping assertions. No live AWS anywhere near the tests.
type stubSSM struct {
	pages   []*ssm.GetParametersByPathOutput
	listErr error

	listCalls   []*ssm.GetParametersByPathInput
	deleteCalls []*ssm.DeleteParametersInput

	deleteOut *ssm.DeleteParametersOutput
	deleteErr error
}

var _ engine.SSMAPI = (*stubSSM)(nil)

func (s *stubSSM) GetParametersByPath(
	_ context.Context, params *ssm.GetParametersByPathInput, _ ...func(*ssm.Options),
) (*ssm.GetParametersByPathOutput, error) {
	s.listCalls = append(s.listCalls, params)

	if s.listErr != nil {
		return nil, s.listErr
	}

	page := len(s.listCalls) - 1
	if page >= len(s.pages) {
		panic("stubSSM: paginator asked past the scripted pages")
	}

	return s.pages[page], nil
}

func (s *stubSSM) DeleteParameters(
	_ context.Context, params *ssm.DeleteParametersInput, _ ...func(*ssm.Options),
) (*ssm.DeleteParametersOutput, error) {
	s.deleteCalls = append(s.deleteCalls, params)

	if s.deleteErr != nil {
		return nil, s.deleteErr
	}

	if s.deleteOut != nil {
		return s.deleteOut, nil
	}

	return &ssm.DeleteParametersOutput{}, nil
}

// param builds one well-formed listing row.
func param(name string, modified time.Time) ssmtypes.Parameter {
	return ssmtypes.Parameter{Name: aws.String(name), LastModifiedDate: aws.Time(modified)}
}

func TestAWSSSMClientListShapesAndPaginates(t *testing.T) {
	t.Parallel()

	stub := &stubSSM{pages: []*ssm.GetParametersByPathOutput{
		{
			Parameters: []ssmtypes.Parameter{
				param("/test/emp-alice/demo/db-url", now.Add(-2*time.Hour)),
				param("/test/emp-alice/demo/db-pass", now.Add(-time.Hour)),
			},
			NextToken: aws.String("page-2"),
		},
		{
			Parameters: []ssmtypes.Parameter{
				param("/test/ci-7/run/token", now.Add(-30*time.Minute)),
			},
		},
	}}
	client := &engine.AWSSSMClient{API: stub}

	var seen []engine.Parameter

	err := client.ListParameters(context.Background(), "/test/", func(p engine.Parameter) error {
		seen = append(seen, p)

		return nil
	})
	require.NoError(t, err)

	// Every page's rows arrive, in order, with both fields carried over.
	require.Len(t, seen, 3)
	assert.Equal(t, "/test/emp-alice/demo/db-url", seen[0].Name)
	assert.Equal(t, now.Add(-2*time.Hour), seen[0].LastModified)
	assert.Equal(t, "/test/ci-7/run/token", seen[2].Name)

	// Request shaping: the root as Path, recursive, and the page token
	// threaded from the first response into the second request.
	require.Len(t, stub.listCalls, 2)
	assert.Equal(t, "/test/", aws.ToString(stub.listCalls[0].Path))
	assert.True(t, aws.ToBool(stub.listCalls[0].Recursive), "listing must be recursive: tenants nest as /test/<ns>/<rel>/...")
	assert.Nil(t, stub.listCalls[0].NextToken)
	assert.Equal(t, "page-2", aws.ToString(stub.listCalls[1].NextToken))
}

func TestAWSSSMClientListSkipsIncompleteRows(t *testing.T) {
	t.Parallel()

	stub := &stubSSM{pages: []*ssm.GetParametersByPathOutput{{
		Parameters: []ssmtypes.Parameter{
			{Name: nil, LastModifiedDate: aws.Time(now)},
			{Name: aws.String("/test/emp-alice/demo/undated"), LastModifiedDate: nil},
			param("/test/emp-alice/demo/whole", now),
		},
	}}}
	client := &engine.AWSSSMClient{API: stub}

	var seen []string

	err := client.ListParameters(context.Background(), "/test/", func(p engine.Parameter) error {
		seen = append(seen, p.Name)

		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"/test/emp-alice/demo/whole"}, seen)
}

func TestAWSSSMClientListVisitErrorStopsListing(t *testing.T) {
	t.Parallel()

	boom := errors.New("visit refused")
	stub := &stubSSM{pages: []*ssm.GetParametersByPathOutput{
		{
			Parameters: []ssmtypes.Parameter{param("/test/emp-alice/demo/a", now)},
			NextToken:  aws.String("page-2"),
		},
		{Parameters: []ssmtypes.Parameter{param("/test/emp-alice/demo/b", now)}},
	}}
	client := &engine.AWSSSMClient{API: stub}

	err := client.ListParameters(context.Background(), "/test/", func(engine.Parameter) error { return boom })
	require.ErrorIs(t, err, boom)
	assert.Len(t, stub.listCalls, 1, "a refusing visitor must stop the listing, not just skip the row")
}

func TestAWSSSMClientListAPIErrorNamesRoot(t *testing.T) {
	t.Parallel()

	stub := &stubSSM{listErr: errors.New("AccessDeniedException")}
	client := &engine.AWSSSMClient{API: stub}

	err := client.ListParameters(context.Background(), "/test/", func(engine.Parameter) error { return nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/test/", "the error must name the root it was listing")
	assert.Contains(t, err.Error(), "AccessDeniedException")
}

func TestAWSSSMClientDeleteBatchesOfTen(t *testing.T) {
	t.Parallel()

	names := make([]string, 25)
	for i := range names {
		names[i] = fmt.Sprintf("/test/emp-alice/demo/p%02d", i)
	}

	stub := &stubSSM{}
	client := &engine.AWSSSMClient{API: stub}

	require.NoError(t, client.DeleteParameters(context.Background(), names))

	// The DeleteParameters API caps at 10 names per call.
	require.Len(t, stub.deleteCalls, 3)
	assert.Len(t, stub.deleteCalls[0].Names, 10)
	assert.Len(t, stub.deleteCalls[1].Names, 10)
	assert.Len(t, stub.deleteCalls[2].Names, 5)

	var sent []string
	for _, call := range stub.deleteCalls {
		sent = append(sent, call.Names...)
	}

	assert.Equal(t, names, sent, "batching must preserve every name in order")
}

func TestAWSSSMClientDeleteRefusesOutsideTestRoot(t *testing.T) {
	t.Parallel()

	stub := &stubSSM{}
	client := &engine.AWSSSMClient{API: stub}

	err := client.DeleteParameters(context.Background(), []string{"/test/emp-alice/demo/ok", "/secrets/mirror/key"})
	require.ErrorIs(t, err, engine.ErrOutOfReach)
	assert.Empty(t, stub.deleteCalls, "one out-of-root name must refuse the whole call before any API request")
}

func TestAWSSSMClientDeleteSurfacesInvalidParameters(t *testing.T) {
	t.Parallel()

	stub := &stubSSM{deleteOut: &ssm.DeleteParametersOutput{InvalidParameters: []string{"/test/emp-alice/demo/gone"}}}
	client := &engine.AWSSSMClient{API: stub}

	err := client.DeleteParameters(context.Background(), []string{"/test/emp-alice/demo/gone"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/test/emp-alice/demo/gone")
}

func TestAWSSSMClientDeleteAPIError(t *testing.T) {
	t.Parallel()

	boom := errors.New("ThrottlingException")
	stub := &stubSSM{deleteErr: boom}
	client := &engine.AWSSSMClient{API: stub}

	err := client.DeleteParameters(context.Background(), []string{"/test/emp-alice/demo/a"})
	require.ErrorIs(t, err, boom)
}
