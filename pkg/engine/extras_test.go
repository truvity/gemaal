package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/gemaal/pkg/engine"
)

func TestExecRunnerOutput(t *testing.T) {
	out, err := engine.ExecRunner{}.Output(context.Background(), "sh", "-c", "echo hello")
	require.NoError(t, err)
	assert.Equal(t, "hello\n", out)

	_, err = engine.ExecRunner{}.Output(context.Background(), "sh", "-c", "echo broken >&2; exit 3")
	require.ErrorContains(t, err, "broken", "stderr folds into the error")
}

func TestViewFind(t *testing.T) {
	view := &engine.View{Tenants: []engine.TenantState{
		{Tenant: engine.Tenant{Namespace: "emp-jdoe", Release: "myapp"}, Tier: "employee"},
	}}

	state, found := view.Find("emp-jdoe", "myapp")
	require.True(t, found)
	assert.Equal(t, "employee", state.Tier)

	_, found = view.Find("emp-jdoe", "ghost")
	assert.False(t, found)
}

func TestPlanEmpty(t *testing.T) {
	assert.True(t, (&engine.Plan{}).Empty())
	assert.False(t, (&engine.Plan{Delete: []engine.Action{{}}}).Empty())
}

func TestExecuteWithoutClientsFails(t *testing.T) {
	// A confirmed apply against a plan whose kind has no client wired
	// records the failure — it never silently succeeds.
	eng := newEngine(testConfig(t), &fakeKube{}, &fakeHelm{}, nil, nil)

	plan := &engine.Plan{
		Namespaces: []string{"emp-gone"},
		Delete: []engine.Action{{
			Item: engine.Item{
				Kind:   engine.KindSSMPrefix,
				Tenant: engine.Tenant{Namespace: "emp-gone", Release: "myapp"},
				ID:     "/test/emp-gone/myapp/",
				Names:  []string{"/test/emp-gone/myapp/db-url"},
			},
			Rule: engine.RuleOrphanGrace,
		}},
	}

	record, err := eng.Apply(context.Background(), plan, true, "rpc")
	require.ErrorContains(t, err, "no ssm client")
	require.Len(t, record.Results, 1)
	assert.False(t, record.Results[0].Executed)
}

func TestAuthorizeS3Actions(t *testing.T) {
	cfg := testConfig(t)
	cfg.AllowList.S3Buckets = []string{"truvity-devel-test"}

	s3 := &fakeS3{}
	eng := newEngine(cfg, &fakeKube{}, &fakeHelm{}, nil, s3)

	action := engine.Action{
		Item: engine.Item{
			Kind:   engine.KindS3Prefix,
			Tenant: engine.Tenant{Namespace: "emp-gone", Release: "myapp"},
			ID:     "s3://truvity-devel-test/emp-gone/myapp/",
		},
		Rule: engine.RuleOrphanGrace,
	}

	plan := &engine.Plan{Namespaces: []string{"emp-gone"}, Delete: []engine.Action{action}}

	record, err := eng.Apply(context.Background(), plan, true, "rpc")
	require.NoError(t, err)
	require.Len(t, record.Results, 1)
	assert.True(t, record.Results[0].Executed)
	assert.Equal(t, []string{"truvity-devel-test/emp-gone/myapp/"}, s3.deleted)

	// A prefix in a bucket outside the allow-list is unreachable.
	foreign := plan
	foreign.Delete[0].Item.ID = "s3://prod-bucket/emp-gone/myapp/"

	_, err = eng.Apply(context.Background(), foreign, true, "rpc")
	require.ErrorIs(t, err, engine.ErrOutOfReach)
}

func TestAuthorizeUnknownKindRefused(t *testing.T) {
	eng := newEngine(testConfig(t), &fakeKube{}, &fakeHelm{}, nil, nil)

	plan := &engine.Plan{Delete: []engine.Action{{Item: engine.Item{Kind: "cnpg-database"}}}}

	_, err := eng.Apply(context.Background(), plan, true, "rpc")
	require.ErrorIs(t, err, engine.ErrOutOfReach)
}

func TestAuthorizeForeignReleaseInTenant(t *testing.T) {
	// A release action whose ID does not belong to its own tenant is a
	// malformed plan, refused.
	eng := newEngine(testConfig(t), &fakeKube{}, &fakeHelm{}, nil, nil)

	plan := &engine.Plan{
		Namespaces: []string{"emp-jdoe"},
		Delete: []engine.Action{{
			Item: engine.Item{
				Kind:   engine.KindHelmRelease,
				Tenant: engine.Tenant{Namespace: "emp-jdoe", Release: "myapp"},
				ID:     "emp-jdoe/otherapp",
			},
			Rule: engine.RuleTTLExpired,
		}},
	}

	_, err := eng.Apply(context.Background(), plan, true, "rpc")
	require.ErrorIs(t, err, engine.ErrOutOfReach)
}

func TestAWSSSMDeleteRefusesOutsideTestRoot(t *testing.T) {
	// The last-moment /test/ recheck fires before any API use — the
	// client is deliberately constructed without one.
	client := &engine.AWSSSMClient{}

	err := client.DeleteParameters(context.Background(), []string{"/secrets/prod/key"})
	require.ErrorIs(t, err, engine.ErrOutOfReach)

	err = client.DeleteParameters(context.Background(), nil)
	require.NoError(t, err, "nothing to delete is not an error")
}

func TestEngineDefaults(t *testing.T) {
	// A bare engine still answers time and swallows logs — the wiring
	// may omit both.
	eng := &engine.Engine{Config: testConfig(t), Kube: &fakeKube{}, Helm: &fakeHelm{}}

	view, err := eng.View(context.Background())
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), view.At, time.Minute)

	_, err = eng.Apply(context.Background(), &engine.Plan{}, false, "loop")
	require.NoError(t, err)
}

func TestHistoryDefaultCapacity(t *testing.T) {
	h := engine.NewHistory(0)

	for range 120 {
		h.Add(engine.SweepRecord{})
	}

	assert.Len(t, h.List(), 100)
}
