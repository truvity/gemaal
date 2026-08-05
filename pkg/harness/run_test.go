package harness

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMain stands in for *testing.M.
type fakeMain struct {
	code int
	ran  bool
}

func (m *fakeMain) Run() int {
	m.ran = true

	return m.code
}

// suiteRecorder builds hooks that append their step name to a shared
// trace — phase-order and skip assertions read the trace.
type suiteRecorder struct {
	trace []string
}

func (r *suiteRecorder) hook(name string) Hook {
	return r.failing(name, nil)
}

func (r *suiteRecorder) failing(name string, err error) Hook {
	return func(context.Context, *Cluster, Tenant) error {
		r.trace = append(r.trace, name)

		return err
	}
}

// standing is the resolution every Run test pins: no cluster calls, no
// identity rung.
func standing() Options {
	return Options{Namespace: "emp-jdoe", Release: "myapp", Kubecontext: "devel@oidc"}
}

func TestRunZeroSuiteExportsAndPassesTheCodeThrough(t *testing.T) {
	clearTenantEnv(t)

	m := &fakeMain{code: 3}

	code := Run(m, Suite{Options: standing()})

	assert.Equal(t, 3, code)
	assert.True(t, m.ran)
	assert.Equal(t, "emp-jdoe", os.Getenv(EnvNamespace))
	assert.Equal(t, "myapp", os.Getenv(EnvRelease))
	assert.Equal(t, "devel@oidc", os.Getenv(EnvKubecontext))
}

func TestRunRefusesAnUnresolvableTenant(t *testing.T) {
	clearTenantEnv(t)

	m := &fakeMain{}

	// A namespace but no release: resolution fails before the tests run.
	code := Run(m, Suite{Options: Options{Namespace: "emp-jdoe"}})

	assert.Equal(t, 1, code)
	assert.False(t, m.ran, "the tests must not run inside an unresolved tenant")
}

func TestRunPhaseOrder(t *testing.T) {
	clearTenantEnv(t)

	r := &suiteRecorder{}
	m := &fakeMain{}

	code := Run(m, Suite{
		Options:  standing(),
		Build:    r.hook("build"),
		Deploy:   r.hook("deploy"),
		Setup:    []Hook{r.hook("wait"), r.hook("urls")},
		Teardown: []Hook{r.hook("uninstall-app"), r.hook("uninstall-infra")},
	})

	assert.Equal(t, 0, code)
	assert.True(t, m.ran)
	assert.Equal(t, []string{"build", "deploy", "wait", "urls", "uninstall-app", "uninstall-infra"}, r.trace,
		"build → deploy → setup before m.Run, teardown after, all in declared order")
}

func TestRunHandsTheClusterAndTenantToHooks(t *testing.T) {
	clearTenantEnv(t)

	var (
		gotCluster *Cluster
		gotTenant  Tenant
	)

	code := Run(&fakeMain{}, Suite{
		Options: standing(),
		Setup: []Hook{func(_ context.Context, cluster *Cluster, tenant Tenant) error {
			gotCluster, gotTenant = cluster, tenant

			return nil
		}},
	})

	assert.Equal(t, 0, code)
	require.NotNil(t, gotCluster)
	assert.Equal(t, "devel@oidc", gotCluster.Kubecontext, "the default toolbox derives from the Options")
	assert.Equal(t, Tenant{Namespace: "emp-jdoe", Release: "myapp"}, gotTenant)
}

func TestRunSkipFlags(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{
			name: "skip build keeps deploying",
			env:  map[string]string{EnvSkipBuild: "1"},
			want: []string{"deploy", "setup", "teardown"},
		},
		{
			name: "skip deploy implies skip build, setup and teardown still run",
			env:  map[string]string{EnvSkipDeploy: "true"},
			want: []string{"setup", "teardown"},
		},
		{
			name: "skip destroy keeps the releases",
			env:  map[string]string{EnvSkipDestroy: "1"},
			want: []string{"build", "deploy", "setup"},
		},
		{
			name: "keep is skip destroy's alias",
			env:  map[string]string{EnvKeep: "1"},
			want: []string{"build", "deploy", "setup"},
		},
		{
			name: "explicit false is not a skip",
			env:  map[string]string{EnvSkipDeploy: "0", EnvKeep: "false"},
			want: []string{"build", "deploy", "setup", "teardown"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearTenantEnv(t)

			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			r := &suiteRecorder{}
			m := &fakeMain{}

			code := Run(m, Suite{
				Options:  standing(),
				Build:    r.hook("build"),
				Deploy:   r.hook("deploy"),
				Setup:    []Hook{r.hook("setup")},
				Teardown: []Hook{r.hook("teardown")},
			})

			assert.Equal(t, 0, code)
			assert.True(t, m.ran)
			assert.Equal(t, tt.want, r.trace)
		})
	}
}

func TestRunRefusesAnUnparseableSkipFlag(t *testing.T) {
	clearTenantEnv(t)
	t.Setenv(EnvKeep, "yes") // not a strconv.ParseBool value

	r := &suiteRecorder{}
	m := &fakeMain{}

	code := Run(m, Suite{
		Options:  standing(),
		Deploy:   r.hook("deploy"),
		Teardown: []Hook{r.hook("teardown")},
	})

	assert.Equal(t, 1, code)
	assert.False(t, m.ran)
	assert.Empty(t, r.trace,
		"a misread KEEP would destroy what the operator asked to keep — refuse before touching anything")
}

func TestRunDeployFailureStillTearsDown(t *testing.T) {
	clearTenantEnv(t)

	r := &suiteRecorder{}
	m := &fakeMain{}

	code := Run(m, Suite{
		Options:  standing(),
		Build:    r.hook("build"),
		Deploy:   r.failing("deploy", errors.New("helm exploded")),
		Setup:    []Hook{r.hook("setup")},
		Teardown: []Hook{r.hook("teardown")},
	})

	assert.Equal(t, 1, code)
	assert.False(t, m.ran, "tests must not run against a failed deploy")
	assert.Equal(t, []string{"build", "deploy", "teardown"}, r.trace,
		"a partial install is torn down; setup is not reached")
}

func TestRunSetupFailureStillTearsDown(t *testing.T) {
	clearTenantEnv(t)

	r := &suiteRecorder{}
	m := &fakeMain{}

	code := Run(m, Suite{
		Options:  standing(),
		Setup:    []Hook{r.failing("setup", errors.New("service never became ready"))},
		Teardown: []Hook{r.hook("teardown")},
	})

	assert.Equal(t, 1, code)
	assert.False(t, m.ran)
	assert.Equal(t, []string{"setup", "teardown"}, r.trace)
}

func TestRunTeardownErrorDoesNotOverrideTheTestVerdict(t *testing.T) {
	clearTenantEnv(t)

	r := &suiteRecorder{}
	m := &fakeMain{code: 0}

	code := Run(m, Suite{
		Options:  standing(),
		Teardown: []Hook{r.failing("teardown-1", errors.New("finalizer stuck")), r.hook("teardown-2")},
	})

	assert.Equal(t, 0, code,
		"a failed local teardown leaves a standing tenant for the service to drain — it is reported, not a verdict")
	assert.Equal(t, []string{"teardown-1", "teardown-2"}, r.trace,
		"every teardown hook is attempted even when an earlier one fails")
}

func TestParseSkipFlags(t *testing.T) {
	clearTenantEnv(t)
	t.Setenv(EnvSkipDeploy, "1")

	flags, err := ParseSkipFlags()
	require.NoError(t, err)
	assert.True(t, flags.Deploy)
	assert.True(t, flags.Build, "skipping deploy implies skipping the build that only feeds it")
	assert.False(t, flags.Destroy)
}

func TestSuiteTeardownTimeoutDefault(t *testing.T) {
	assert.Equal(t, DefaultTeardownTimeout, Suite{}.teardownTimeout())
	assert.Equal(t, time.Minute, Suite{TeardownTimeout: time.Minute}.teardownTimeout())
}
