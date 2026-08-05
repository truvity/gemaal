package pipeline

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPushPreviewHappyPath(t *testing.T) {
	p, s, root, _ := newTestPipeline(t)
	chartsOut := filepath.Join(root, "dist", "url-shortener", "charts")

	seedCharts(t, root, p.cfg, "1.2.3", StampPreview)

	s.on("aws ecr get-login-password --profile preview@power --region eu-central-1", stubResult{out: "sekret\n"})

	require.NoError(t, p.PushPreview(context.Background()))

	// Checks run before credentials, login before pushes, ring2 before
	// ring3 (least-consumable artifact first).
	login := s.find("helm registry login --username AWS --password-stdin preview.example.com")
	ring2 := s.find("go tool helmctl push --tgz")
	require.GreaterOrEqual(t, login, 0, "calls: %v", s.joinedCalls())
	require.GreaterOrEqual(t, ring2, 0)
	assert.Less(t, s.find("aws ecr get-login-password"), login)
	assert.Less(t, login, ring2)

	// The password is piped into helm login verbatim.
	loginCmd := s.call(t, "helm registry login")
	require.NotNil(t, loginCmd.Stdin)
	piped, err := io.ReadAll(loginCmd.Stdin)
	require.NoError(t, err)
	assert.Equal(t, "sekret\n", string(piped))

	// Both rings pushed, in order, from the STAGED snapshot — never from
	// the shared output dir a concurrent build could rewrite.
	var pushes []Command

	for _, c := range s.calls {
		if strings.HasPrefix(strings.Join(c.Argv, " "), "go tool helmctl push") {
			pushes = append(pushes, c)
		}
	}

	require.Len(t, pushes, 2)

	argOf := func(c Command, flag string) string {
		for i, a := range c.Argv {
			if a == flag && i+1 < len(c.Argv) {
				return c.Argv[i+1]
			}
		}

		return ""
	}

	assert.Equal(t, "url-shortener/charts/url-shortener-infra", argOf(pushes[0], "--repository"))
	assert.Equal(t, "url-shortener/charts/url-shortener", argOf(pushes[1], "--repository"))
	assert.Equal(t, "url-shortener-infra", argOf(pushes[0], "--name"))
	assert.Equal(t, "1.2.3", argOf(pushes[0], "--version"))

	for _, push := range pushes {
		tgz := argOf(push, "--tgz")
		require.NotEmpty(t, tgz)
		assert.NotContains(t, tgz, chartsOut, "push must read the staged snapshot, not the shared dir")
	}

	// The transient stage is removed on exit.
	assert.NoDirExists(t, filepath.Dir(argOf(pushes[0], "--tgz")))
}

// TestPushPreviewRefusals is the table over every pre-credential
// refusal: no charts, unknown provenance, wrong stamp, incoherent pair.
// In every case NOTHING may reach a credential — no aws, no helm login,
// no push.
func TestPushPreviewRefusals(t *testing.T) {
	cases := []struct {
		name       string
		seed       func(t *testing.T, root string, cfg *Config)
		wantErr    string
		wantStderr []string
	}{
		{
			name:       "charts directory missing",
			seed:       func(*testing.T, string, *Config) {},
			wantErr:    "does not exist",
			wantStderr: []string{"no packaged charts to push", "hint: rebuild with 'gemaalctl pipeline snapshot'"},
		},
		{
			name: "stamp missing — unknown provenance",
			seed: func(t *testing.T, root string, cfg *Config) {
				seedCharts(t, root, cfg, "1.2.3", "")
			},
			wantErr:    "unknown provenance",
			wantStderr: []string{"have unknown provenance"},
		},
		{
			// The stamp mismatch refusal: stable-stamped charts embed the
			// stable registry in their digest-pinned image refs — pushing
			// them to preview would point consumers at the wrong account.
			name: "stable-stamped charts refused",
			seed: func(t *testing.T, root string, cfg *Config) {
				seedCharts(t, root, cfg, "1.2.3", StampStable)
			},
			wantErr: `charts are stamped "stable"`,
			wantStderr: []string{
				"packaged charts are stamped 'stable', but this is the preview-only push",
				"digest-pinned image refs (registry included)",
				"'gemaalctl pipeline release-stable'",
				"hint: rebuild for preview with 'gemaalctl pipeline snapshot'",
			},
		},
		{
			name: "incoherent pair refused before provenance",
			seed: func(t *testing.T, root string, cfg *Config) {
				chartsOut := filepath.Join(root, filepath.FromSlash(cfg.ChartsOut()))
				writeChartTgz(t, filepath.Join(chartsOut, "url-shortener-infra-1.2.3.tgz"), "url-shortener-infra", "1.2.3")
				writeChartTgz(t, filepath.Join(chartsOut, "url-shortener-2.0.0.tgz"), "url-shortener", "2.0.0")
				require.NoError(t, os.WriteFile(filepath.Join(chartsOut, stampFileName), []byte("preview\n"), 0o644))
			},
			wantErr:    "incoherent chart pair",
			wantStderr: []string{"the rings disagree on the version"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, s, root, stderr := newTestPipeline(t)
			tc.seed(t, root, p.cfg)

			err := p.PushPreview(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)

			for _, want := range tc.wantStderr {
				assert.Contains(t, stderr.String(), want)
			}

			// A bad chart directory never reaches a credential.
			assert.False(t, s.called("aws"))
			assert.False(t, s.called("helm registry login"))
			assert.False(t, s.called("go tool helmctl push"))
		})
	}
}

// TestPushPreviewErrorsNameSharedDir proves refusal texts point the
// operator at the real charts dir (which a rebuild repopulates), not at
// the transient stage.
func TestPushPreviewErrorsNameSharedDir(t *testing.T) {
	p, _, root, stderr := newTestPipeline(t)

	chartsOut := filepath.Join(root, filepath.FromSlash(p.cfg.ChartsOut()))
	writeChartTgz(t, filepath.Join(chartsOut, "url-shortener-infra-1.2.3.tgz"), "url-shortener-infra", "1.2.3")

	err := p.PushPreview(context.Background())
	require.Error(t, err)
	assert.Contains(t, stderr.String(), "dist/url-shortener/charts")
	assert.NotContains(t, stderr.String(), "gemaal-charts-stage-")
}
