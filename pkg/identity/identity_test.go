package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubRunner scripts one command's outcome and records every invocation.
type stubRunner struct {
	out   string
	err   error
	calls [][]string
}

func (s *stubRunner) Output(_ context.Context, argv ...string) (string, error) {
	s.calls = append(s.calls, argv)

	return s.out, s.err
}

func TestEnvDriver(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		wantEmail  string
		noEvidence bool
		wantErr    string
	}{
		{name: "set", value: "j.doe@example.com", wantEmail: "j.doe@example.com"},
		{name: "whitespace trimmed", value: "  j.doe@example.com\n", wantEmail: "j.doe@example.com"},
		{name: "unset is no evidence", value: "", noEvidence: true},
		{name: "non-email is fatal", value: "jdoe", wantErr: "not an email"},
		{name: "missing local part is fatal", value: "@example.com", wantErr: "not an email"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvEmail, tt.value)

			email, err := EnvDriver{}.Email(context.Background())

			switch {
			case tt.noEvidence:
				require.ErrorIs(t, err, ErrNoEvidence)
			case tt.wantErr != "":
				require.Error(t, err)
				assert.NotErrorIs(t, err, ErrNoEvidence, "a set-but-invalid override must stop the chain")
				assert.Contains(t, err.Error(), tt.wantErr)
			default:
				require.NoError(t, err)
				assert.Equal(t, tt.wantEmail, email)
			}
		})
	}
}

func TestEnvDriverCustomVar(t *testing.T) {
	t.Setenv("OTHER_EMAIL", "j.doe@example.com")
	t.Setenv(EnvEmail, "wrong@example.com")

	email, err := EnvDriver{Var: "OTHER_EMAIL"}.Email(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "j.doe@example.com", email)
}

func whoamiJSON(username string) string {
	return fmt.Sprintf(`{"kind":"SelfSubjectReview","status":{"userInfo":{"username":%q,"groups":["a","b"]}}}`, username)
}

func TestKubectlDriver(t *testing.T) {
	tests := []struct {
		name       string
		out        string
		err        error
		wantEmail  string
		noEvidence bool
		wantErr    string
	}{
		{name: "email username", out: whoamiJSON("j.doe@example.com"), wantEmail: "j.doe@example.com"},
		{name: "kubectl failure is no evidence", err: errors.New("no such context"), noEvidence: true},
		{name: "service account is no evidence", out: whoamiJSON("system:serviceaccount:ci:runner"), noEvidence: true},
		{name: "certificate CN is no evidence", out: whoamiJSON("kubernetes-admin"), noEvidence: true},
		{name: "empty username is no evidence", out: whoamiJSON(""), noEvidence: true},
		{name: "garbage output is fatal", out: "not json", wantErr: "parse kubectl auth whoami"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &stubRunner{out: tt.out, err: tt.err}
			d := KubectlDriver{Runner: s}

			email, err := d.Email(context.Background())

			switch {
			case tt.noEvidence:
				require.ErrorIs(t, err, ErrNoEvidence)
			case tt.wantErr != "":
				require.Error(t, err)
				assert.NotErrorIs(t, err, ErrNoEvidence)
				assert.Contains(t, err.Error(), tt.wantErr)
			default:
				require.NoError(t, err)
				assert.Equal(t, tt.wantEmail, email)
			}
		})
	}
}

func TestKubectlDriverArgv(t *testing.T) {
	tests := []struct {
		name   string
		driver KubectlDriver
		want   []string
	}{
		{
			name:   "bare",
			driver: KubectlDriver{},
			want:   []string{"kubectl", "auth", "whoami", "-o", "json"},
		},
		{
			name:   "context and kubeconfig",
			driver: KubectlDriver{Kubecontext: "devel@oidc", Kubeconfig: "/tmp/kc"},
			want:   []string{"kubectl", "--context", "devel@oidc", "--kubeconfig", "/tmp/kc", "auth", "whoami", "-o", "json"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &stubRunner{out: whoamiJSON("j.doe@example.com")}
			tt.driver.Runner = s

			_, err := tt.driver.Email(context.Background())
			require.NoError(t, err)
			require.Len(t, s.calls, 1)
			assert.Equal(t, tt.want, s.calls[0])
		})
	}
}

func callerIdentityJSON(arn string) string {
	return fmt.Sprintf(`{"UserId":"AROAEXAMPLE:session","Account":"123456789012","Arn":%q}`, arn)
}

const ssoARN = "arn:aws:sts::123456789012:assumed-role/AWSReservedSSO_AdministratorAccess_0123456789abcdef/j.doe@example.com"

func TestSessionEmailFromARN(t *testing.T) {
	tests := []struct {
		name       string
		arn        string
		wantEmail  string
		noEvidence bool
	}{
		{name: "sso assumed role", arn: ssoARN, wantEmail: "j.doe@example.com"},
		{
			name:      "plain assumed role with email session",
			arn:       "arn:aws:sts::123456789012:assumed-role/PowerUser/j.doe@example.com",
			wantEmail: "j.doe@example.com",
		},
		{name: "iam user", arn: "arn:aws:iam::123456789012:user/jdoe", noEvidence: true},
		{name: "federated user", arn: "arn:aws:sts::123456789012:federated-user/jdoe", noEvidence: true},
		{name: "role without session segment", arn: "arn:aws:sts::123456789012:assumed-role/PowerUser", noEvidence: true},
		{name: "machine session name", arn: "arn:aws:sts::123456789012:assumed-role/ci-role/gha-run-42", noEvidence: true},
		{name: "empty", arn: "", noEvidence: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			email, err := SessionEmailFromARN(tt.arn)

			if tt.noEvidence {
				require.ErrorIs(t, err, ErrNoEvidence)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantEmail, email)
		})
	}
}

func TestAWSDriver(t *testing.T) {
	t.Run("happy path and argv", func(t *testing.T) {
		s := &stubRunner{out: callerIdentityJSON(ssoARN)}

		email, err := AWSDriver{Runner: s}.Email(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "j.doe@example.com", email)
		require.Len(t, s.calls, 1)
		assert.Equal(t, []string{"aws", "sts", "get-caller-identity", "--output", "json"}, s.calls[0])
	})

	t.Run("profile rides the argv", func(t *testing.T) {
		s := &stubRunner{out: callerIdentityJSON(ssoARN)}

		_, err := AWSDriver{Profile: "sandbox@power", Runner: s}.Email(context.Background())
		require.NoError(t, err)
		assert.Equal(t,
			[]string{"aws", "sts", "get-caller-identity", "--output", "json", "--profile", "sandbox@power"},
			s.calls[0])
	})

	t.Run("cli failure is no evidence", func(t *testing.T) {
		s := &stubRunner{err: errors.New("Unable to locate credentials")}

		_, err := AWSDriver{Runner: s}.Email(context.Background())
		require.ErrorIs(t, err, ErrNoEvidence)
	})

	t.Run("garbage output is fatal", func(t *testing.T) {
		s := &stubRunner{out: "not json"}

		_, err := AWSDriver{Runner: s}.Email(context.Background())
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrNoEvidence)
	})
}

func TestStatic(t *testing.T) {
	email, err := Static("j.doe@example.com").Email(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "j.doe@example.com", email)

	_, err = Static("").Email(context.Background())
	require.ErrorIs(t, err, ErrNoEvidence)
}

// scriptedDriver scripts one driver's outcome for the chain tests.
type scriptedDriver struct {
	name  string
	email string
	err   error
}

func (d scriptedDriver) Name() string                          { return d.name }
func (d scriptedDriver) Email(context.Context) (string, error) { return d.email, d.err }

func TestChainResolve(t *testing.T) {
	noEvidence := func(name string) Driver {
		return scriptedDriver{name: name, err: fmt.Errorf("%w (nothing here)", ErrNoEvidence)}
	}

	t.Run("first email wins, later drivers not consulted", func(t *testing.T) {
		t.Setenv(EnvEmail, "ci@example.com")

		s := &stubRunner{err: errors.New("must not run")}
		chain := Chain{EnvDriver{}, KubectlDriver{Runner: s}, AWSDriver{Runner: s}}

		res, err := chain.Resolve(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "ci@example.com", res.Email)
		assert.Equal(t, "env", res.Driver)
		assert.Empty(t, s.calls, "precedence: the chain must stop at the first answer")
		require.Len(t, res.Attempts, 1)
	})

	t.Run("no-evidence drivers are skipped with a trail", func(t *testing.T) {
		chain := Chain{noEvidence("env"), scriptedDriver{name: "kubectl", email: "j.doe@example.com"}}

		res, err := chain.Resolve(context.Background())
		require.NoError(t, err)
		assert.Equal(t, "j.doe@example.com", res.Email)
		assert.Equal(t, "kubectl", res.Driver)
		require.Len(t, res.Attempts, 2)
		assert.ErrorIs(t, res.Attempts[0].Err, ErrNoEvidence)
		assert.Equal(t, "j.doe@example.com", res.Attempts[1].Email)
	})

	t.Run("all silent is an error naming every driver", func(t *testing.T) {
		chain := Chain{noEvidence("env"), noEvidence("kubectl"), noEvidence("aws")}

		res, err := chain.Resolve(context.Background())
		require.Error(t, err)

		for _, name := range []string{"env", "kubectl", "aws"} {
			assert.Contains(t, err.Error(), name)
		}

		assert.Len(t, res.Attempts, 3, "the failed resolution still carries the full trail")
	})

	t.Run("a broken driver stops the chain", func(t *testing.T) {
		fatal := scriptedDriver{name: "env", err: errors.New("override is garbage")}
		next := scriptedDriver{name: "kubectl", email: "sneaky@example.com"}

		res, err := Chain{fatal, next}.Resolve(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "override is garbage")
		assert.Empty(t, res.Email, "a fatal driver must not hand the answer to the next one")
		require.Len(t, res.Attempts, 1)
	})

	t.Run("empty chain refuses", func(t *testing.T) {
		_, err := Chain{}.Resolve(context.Background())
		require.Error(t, err)
	})
}

func TestDefaultChainOrder(t *testing.T) {
	c := DefaultChain("devel@oidc", "/tmp/kc")

	require.Len(t, c, 3)
	assert.Equal(t, "env", c[0].Name())
	assert.Equal(t, "kubectl", c[1].Name())
	assert.Equal(t, "aws", c[2].Name())

	kd, ok := c[1].(KubectlDriver)
	require.True(t, ok)
	assert.Equal(t, "devel@oidc", kd.Kubecontext)
	assert.Equal(t, "/tmp/kc", kd.Kubeconfig)
}

func TestResolutionTrail(t *testing.T) {
	res := Resolution{Attempts: []Attempt{
		{Driver: "env", Err: fmt.Errorf("%w (GEMAAL_EMAIL unset)", ErrNoEvidence)},
		{Driver: "kubectl", Email: "j.doe@example.com"},
	}}

	trail := res.Trail()
	assert.True(t, strings.HasPrefix(trail, "env: "), trail)
	assert.Contains(t, trail, "kubectl: j.doe@example.com")
}

func TestMapResolver(t *testing.T) {
	m := MapResolver{"J.Doe@Example.com": "jdoe"}

	t.Run("exact", func(t *testing.T) {
		slug, err := m.Resolve(context.Background(), "J.Doe@Example.com")
		require.NoError(t, err)
		assert.Equal(t, "jdoe", slug)
	})

	t.Run("case insensitive", func(t *testing.T) {
		slug, err := m.Resolve(context.Background(), "j.doe@example.com")
		require.NoError(t, err)
		assert.Equal(t, "jdoe", slug)
	})

	t.Run("miss", func(t *testing.T) {
		_, err := m.Resolve(context.Background(), "nobody@example.com")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no slug in the identity map")
	})
}
