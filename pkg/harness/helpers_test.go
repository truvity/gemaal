package harness

import (
	"context"
	"strings"
	"testing"
)

// stubRule scripts one matched invocation.
type stubRule struct {
	prefix string
	out    string
	err    error
}

// stubRunner is the injected executor: rules match on a prefix of the
// space-joined argv, newest rule first. Unmatched commands succeed with
// empty output. Every call is recorded for argv assertions.
type stubRunner struct {
	rules []stubRule
	calls [][]string
}

func (s *stubRunner) on(prefix, out string, err error) {
	s.rules = append([]stubRule{{prefix: prefix, out: out, err: err}}, s.rules...)
}

func (s *stubRunner) exec(argv []string) (string, error) {
	s.calls = append(s.calls, argv)

	joined := strings.Join(argv, " ")
	for _, r := range s.rules {
		if strings.HasPrefix(joined, r.prefix) {
			return r.out, r.err
		}
	}

	return "", nil
}

// Run implements Runner.
func (s *stubRunner) Run(_ context.Context, argv ...string) error {
	_, err := s.exec(argv)

	return err
}

// Output implements Runner.
func (s *stubRunner) Output(_ context.Context, argv ...string) (string, error) {
	return s.exec(argv)
}

// joined returns every recorded invocation as a space-joined string.
func (s *stubRunner) joined() []string {
	out := make([]string, len(s.calls))
	for i, c := range s.calls {
		out[i] = strings.Join(c, " ")
	}

	return out
}

// clearTenantEnv neutralizes every environment rung of the resolution
// ladders and the GEMAAL_TEST_* skip contract, so tests exercise
// exactly the rung they mean to — including on a real CI runner where
// GITHUB_RUN_NUMBER is set for real.
func clearTenantEnv(t *testing.T) {
	t.Helper()

	for _, k := range []string{
		EnvNamespace, EnvRelease, EnvKubecontext, EnvCIRunNumber, EnvCIRunAttempt,
		EnvSkipBuild, EnvSkipDeploy, EnvSkipDestroy, EnvKeep,
	} {
		t.Setenv(k, "")
	}
}
