package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/gemaal/pkg/config"
	"github.com/truvity/gemaal/pkg/service"
)

func TestHelp(t *testing.T) {
	var out bytes.Buffer

	cmd := newCommand()
	cmd.Writer = &out

	require.NoError(t, cmd.Run(context.Background(), []string{"gemaalctl", "--help"}))
	assert.Contains(t, out.String(), "plan")
	assert.Contains(t, out.String(), "checkout")
	assert.Contains(t, out.String(), "extend")
}

func TestVersion(t *testing.T) {
	require.NoError(t, newCommand().Run(context.Background(), []string{"gemaalctl", "version"}))
}

func TestPipelineHelp(t *testing.T) {
	var out bytes.Buffer

	cmd := newCommand()
	cmd.Writer = &out

	require.NoError(t, cmd.Run(context.Background(), []string{"gemaalctl", "pipeline", "--help"}))
	assert.Contains(t, out.String(), "snapshot")
	assert.Contains(t, out.String(), "push-preview")
	assert.Contains(t, out.String(), "release-stable")
}

// TestPipelineRefusesBadConfig proves a flow never starts from an
// unloadable per-project configuration.
func TestPipelineRefusesBadConfig(t *testing.T) {
	err := newCommand().Run(context.Background(), []string{
		"gemaalctl", "pipeline", "snapshot", "--config", "/nonexistent/pipeline.yaml",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read pipeline config")
}

// startStubServer serves the G1 service skeleton for client smoke tests.
func startStubServer(t *testing.T) string {
	t.Helper()

	cfg, err := config.Parse([]byte("{}"))
	require.NoError(t, err)

	svc := service.New(cfg, slog.New(slog.DiscardHandler))

	mux := http.NewServeMux()
	path, handler := svc.Handler()
	mux.Handle(path, handler)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server.URL
}

func TestPlanAgainstStubServer(t *testing.T) {
	url := startStubServer(t)

	err := newCommand().Run(context.Background(), []string{"gemaalctl", "plan", "--server", url})
	require.NoError(t, err)
}

func TestCheckoutIsUnimplementedInG1(t *testing.T) {
	url := startStubServer(t)

	err := newCommand().Run(context.Background(), []string{
		"gemaalctl", "checkout", "--server", url, "--namespace", "emp-jdoe", "--release", "myapp",
	})
	require.Error(t, err)
	assert.Equal(t, connect.CodeUnimplemented, connect.CodeOf(err))
}
