package harness

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/gemaal/pkg/gemaalcfg"
	"github.com/truvity/gemaal/pkg/identity"
)

// explodingDriver is an identity driver that must never be reached.
type explodingDriver struct{}

func (explodingDriver) Name() string { return "exploding" }
func (explodingDriver) Email(context.Context) (string, error) {
	return "", errors.New("the identity rung must not have been consulted")
}

// jdoe wires the identity rung deterministically: static evidence plus a
// map resolver.
func jdoe() (identity.Chain, identity.Resolver) {
	return identity.Chain{identity.Static("j.doe@example.com")},
		identity.MapResolver{"j.doe@example.com": "jdoe"}
}

func TestResolveNamespaceLadder(t *testing.T) {
	chain, resolver := jdoe()

	tests := []struct {
		name    string
		env     map[string]string
		opts    Options
		want    string
		wantErr string
	}{
		{
			name: "options wins over everything",
			env:  map[string]string{EnvNamespace: "from-env"},
			opts: Options{Namespace: "from-options", Chain: identity.Chain{explodingDriver{}}},
			want: "from-options",
		},
		{
			name: "env wins over the identity rung",
			env:  map[string]string{EnvNamespace: "from-env"},
			opts: Options{Chain: identity.Chain{explodingDriver{}}},
			want: "from-env",
		},
		{
			name: "identity rung renders the default template",
			opts: Options{Chain: chain, Resolver: resolver},
			want: "emp-jdoe",
		},
		{
			name: "options template wins over config template",
			opts: Options{
				Chain: chain, Resolver: resolver,
				PersonalNamespace: "dev-{slug}",
				Config:            &gemaalcfg.Config{PersonalNamespace: "team-{slug}"},
			},
			want: "dev-jdoe",
		},
		{
			name: "config template wins over the default",
			opts: Options{Chain: chain, Resolver: resolver, Config: &gemaalcfg.Config{PersonalNamespace: "team-{slug}"}},
			want: "team-jdoe",
		},
		{
			name: "config identity map serves when no resolver is set",
			opts: Options{
				Chain:  chain,
				Config: &gemaalcfg.Config{Identity: gemaalcfg.Identity{Emails: map[string]string{"j.doe@example.com": "jdoe"}}},
			},
			want: "emp-jdoe",
		},
		{
			name:    "unmapped email names the email and the evidence",
			opts:    Options{Chain: chain, Resolver: identity.MapResolver{}},
			wantErr: "j.doe@example.com",
		},
		{
			name:    "no resolver at all says where to put one",
			opts:    Options{Chain: chain},
			wantErr: "Options.Resolver",
		},
		{
			name:    "template without the placeholder refuses",
			opts:    Options{Chain: chain, Resolver: resolver, PersonalNamespace: "emp-static"},
			wantErr: "must contain {slug}",
		},
		{
			name:    "env namespace is still bounds-checked",
			env:     map[string]string{EnvNamespace: "Bad_Namespace"},
			opts:    Options{},
			wantErr: "RFC1123",
		},
		{
			name:    "rendered namespace over 63 characters refuses",
			opts:    Options{Chain: chain, Resolver: resolver, PersonalNamespace: strings.Repeat("x", 62) + "-{slug}"},
			wantErr: "over the 63-character bound",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearTenantEnv(t)

			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			ns, err := ResolveNamespace(context.Background(), tt.opts)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, ns)
		})
	}
}

func TestResolveReleaseLadder(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		opts    Options
		want    string
		wantErr string
	}{
		{
			name: "options wins over env and CI",
			env:  map[string]string{EnvRelease: "from-env", EnvCIRunNumber: "9"},
			opts: Options{Release: "from-options"},
			want: "from-options",
		},
		{
			name: "env wins over CI",
			env:  map[string]string{EnvRelease: "from-env", EnvCIRunNumber: "9"},
			want: "from-env",
		},
		{
			name: "CI renders run and attempt",
			env:  map[string]string{EnvCIRunNumber: "123", EnvCIRunAttempt: "2"},
			opts: Options{App: "myapp"},
			want: "r123-a2",
		},
		{
			name: "CI attempt defaults to 1",
			env:  map[string]string{EnvCIRunNumber: "123"},
			want: "r123-a1",
		},
		{
			name: "app is the standing install's release",
			opts: Options{App: "url-shortener"},
			want: "url-shortener",
		},
		{
			name:    "nothing resolves to a usable message",
			wantErr: EnvRelease,
		},
		{
			name:    "release over the helm bound refuses",
			opts:    Options{Release: strings.Repeat("x", 54)},
			wantErr: "over the 53-character bound",
		},
		{
			name:    "release shape is asserted",
			opts:    Options{Release: "Not-Valid"},
			wantErr: "RFC1123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearTenantEnv(t)

			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			tt.opts.Namespace = "emp-jdoe" // pin the other half

			tenant, err := Resolve(context.Background(), tt.opts)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, tenant.Release)
		})
	}
}

func TestTenantEnvAndExport(t *testing.T) {
	clearTenantEnv(t)

	tenant := Tenant{Namespace: "emp-jdoe", Release: "myapp"}

	assert.Equal(t,
		[]string{"GEMAAL_NAMESPACE=emp-jdoe", "GEMAAL_RELEASE=myapp", "GEMAAL_KUBECONTEXT=devel@oidc"},
		tenant.Env("devel@oidc"))
	assert.Equal(t, []string{"GEMAAL_NAMESPACE=emp-jdoe", "GEMAAL_RELEASE=myapp"}, tenant.Env(""))

	require.NoError(t, tenant.Export("devel@oidc"))
	assert.Equal(t, "emp-jdoe", os.Getenv(EnvNamespace))
	assert.Equal(t, "myapp", os.Getenv(EnvRelease))
	assert.Equal(t, "devel@oidc", os.Getenv(EnvKubecontext))
}

func TestTenantString(t *testing.T) {
	assert.Equal(t, "emp-jdoe/myapp", Tenant{Namespace: "emp-jdoe", Release: "myapp"}.String())
}

// fakeMain stands in for *testing.M.
type fakeMain struct {
	code int
	ran  bool
}

func (m *fakeMain) Run() int {
	m.ran = true

	return m.code
}

func TestRunExportsAndPassesTheCodeThrough(t *testing.T) {
	clearTenantEnv(t)

	m := &fakeMain{code: 3}

	code := Run(m, Options{Namespace: "emp-jdoe", Release: "myapp", Kubecontext: "devel@oidc"})

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
	code := Run(m, Options{Namespace: "emp-jdoe"})

	assert.Equal(t, 1, code)
	assert.False(t, m.ran, "the tests must not run inside an unresolved tenant")
}
