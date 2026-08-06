package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/gemaal/pkg/config"
)

func TestHelp(t *testing.T) {
	var out bytes.Buffer

	cmd := newCommand()
	cmd.Writer = &out

	require.NoError(t, cmd.Run(context.Background(), []string{"gemaal", "--help"}))
	assert.Contains(t, out.String(), "--config")
}

func TestVersion(t *testing.T) {
	require.NoError(t, newCommand().Run(context.Background(), []string{"gemaal", "version"}))
}

func TestMissingConfigRefused(t *testing.T) {
	err := newCommand().Run(context.Background(), []string{"gemaal", "--config", "/does/not/exist.yaml"})
	require.Error(t, err)
}

// TestWireFromExample proves the wiring function and the example
// document stay coherent: everything a deployment assembles — engine,
// authenticator, service, panel — assembles from the committed example.
func TestWireFromExample(t *testing.T) {
	// Outside a cluster the reviewer is skipped with a warning; that
	// path must not depend on this machine's environment.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	cfg, err := config.Load(filepath.Join("..", "..", "config.example.yaml"))
	require.NoError(t, err)

	svc, ui, eng, err := wire(context.Background(), cfg, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	assert.NotNil(t, svc)
	assert.NotNil(t, ui)
	require.NotNil(t, eng)
	assert.NotNil(t, eng.SSM, "the example configures a /test/ root, so the SSM client wires up")
	assert.Nil(t, eng.S3, "the S3 target stays stubbed off pending the shared test bucket")
}
