package pipeline

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// GEMAAL_AWS_AMBIENT drops every profile the config names: no
// AWS_PROFILE in the flow env, no --profile on aws/helmctl — the
// default credential chain (env, container, IMDS) must see no profile
// at all, because an explicit --profile makes the AWS CLI ignore
// ambient credentials entirely. That is the CI face; laptops keep the
// committed profiles.
func TestAmbientDropsProfiles(t *testing.T) {
	cfg := testConfig(t)

	t.Run("default keeps profiles", func(t *testing.T) {
		p := New(cfg, &stubRunner{}, slog.New(slog.DiscardHandler), &bytes.Buffer{})

		env := p.destinationEnv(cfg.Registries.Preview, true)
		require.Contains(t, env, "AWS_PROFILE=preview@power")
		require.Contains(t, env, "REGISTRY=preview.example.com")
		require.Contains(t, env, "KO_DOCKER_REPO=preview.example.com/url-shortener")

		require.Equal(t, []string{"--profile", "preview@power"}, p.profileArgs(cfg.Registries.Preview))
	})

	t.Run("ambient drops them", func(t *testing.T) {
		t.Setenv("GEMAAL_AWS_AMBIENT", "1")
		p := New(cfg, &stubRunner{}, slog.New(slog.DiscardHandler), &bytes.Buffer{})

		env := p.destinationEnv(cfg.Registries.Preview, true)
		for _, e := range env {
			require.NotContains(t, e, "AWS_PROFILE")
		}
		require.Contains(t, env, "REGISTRY=preview.example.com")

		require.Empty(t, p.profileArgs(cfg.Registries.Preview))
	})

	t.Run("ambient ecr login carries no profile", func(t *testing.T) {
		t.Setenv("GEMAAL_AWS_AMBIENT", "true")
		s := &stubRunner{}
		p := New(cfg, s, slog.New(slog.DiscardHandler), &bytes.Buffer{})
		p.root = t.TempDir()

		s.on("aws ecr get-login-password", stubResult{out: "hunter2"})
		require.NoError(t, p.ecrLogin(t.Context(), nil, cfg.Registries.Preview))

		var loginCall string
		for _, c := range s.calls {
			joined := strings.Join(c.Argv, " ")
			if strings.HasPrefix(joined, "aws ecr get-login-password") {
				loginCall = joined
			}
		}
		require.NotEmpty(t, loginCall)
		require.NotContains(t, loginCall, "--profile")
		require.Contains(t, loginCall, "--region eu-central-1")
	})
}
