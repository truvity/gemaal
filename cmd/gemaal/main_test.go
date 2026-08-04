package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
