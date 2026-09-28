package harness

import (
	"context"
	"strings"
	"testing"
)

// stubRule scripts one matched invocation, or a SEQUENCE of them: outs is
// walked in order across repeated calls matching the same prefix, then
// holds on the last entry — a rollout observed mid-flight on one poll and
// finished (or still stuck) on the next.
type stubRule struct {
	prefix string
	outs   []string
	err    error
	calls  int
}

// stubRunner is the injected executor: rules match on a prefix of the
// space-joined argv, newest rule first. Unmatched commands succeed with
// empty output. Every call is recorded for argv assertions.
type stubRunner struct {
	rules []*stubRule
	calls [][]string
}

func (s *stubRunner) on(prefix, out string, err error) {
	s.onSeq(prefix, []string{out}, err)
}

// onSeq scripts a prefix to return each of outs in turn across
// successive matching calls, repeating the last one once exhausted.
func (s *stubRunner) onSeq(prefix string, outs []string, err error) {
	s.rules = append([]*stubRule{{prefix: prefix, outs: outs, err: err}}, s.rules...)
}

func (s *stubRunner) exec(argv []string) (string, error) {
	s.calls = append(s.calls, argv)

	joined := strings.Join(argv, " ")
	for _, r := range s.rules {
		if strings.HasPrefix(joined, r.prefix) {
			i := r.calls
			if i >= len(r.outs) {
				i = len(r.outs) - 1
			}
			r.calls++

			return r.outs[i], r.err
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
		EnvNamespace, EnvRelease, EnvKubecontext, EnvServer, EnvCIRunNumber, EnvCIRunAttempt,
		EnvSkipBuild, EnvSkipDeploy, EnvSkipDestroy, EnvKeep, EnvTier,
	} {
		t.Setenv(k, "")
	}
}
