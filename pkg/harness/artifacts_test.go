// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package harness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChartTgzs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, name := range []string{
		"dms-0.0.1-abc-nightly.tgz",
		"dms-infra-0.0.1-abc-nightly.tgz",
		// The trap this function must not fall into: the app prefix
		// "dms-" also matches the infra chart's name, so matching order
		// decides which is which.
		"unrelated.txt",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
	}

	infra, app, err := ChartTgzs(dir, "dms")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "dms-infra-0.0.1-abc-nightly.tgz"), infra)
	assert.Equal(t, filepath.Join(dir, "dms-0.0.1-abc-nightly.tgz"), app)
}

func TestChartTgzsIncompletePair(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dms-0.0.1.tgz"), nil, 0o600))

	_, _, err := ChartTgzs(dir, "dms")
	require.ErrorContains(t, err, "incomplete")
}

func TestChartTgzsMissingDir(t *testing.T) {
	t.Parallel()

	_, _, err := ChartTgzs(filepath.Join(t.TempDir(), "nope"), "dms")
	require.ErrorContains(t, err, "not found")
}

// The ring2-only lane's whole point: an app chart that was never built
// is the NORMAL state here, not an incomplete pair. ChartTgzs rejects
// this same directory, and must keep doing so -- the two answer
// different questions.
func TestInfraChartTgzWithoutAppChart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dms-infra-0.0.1-abc.tgz"), nil, 0o600))

	infra, err := InfraChartTgz(dir, "dms")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "dms-infra-0.0.1-abc.tgz"), infra)

	_, _, pairErr := ChartTgzs(dir, "dms")
	require.Error(t, pairErr, "ChartTgzs must still refuse a half-built pair")
}

// The app chart must never be mistaken for the infra one: "dms-" is a
// prefix of "dms-infra-", so a careless match returns the wrong chart
// and the lane installs the daemons it exists to skip.
func TestInfraChartTgzIgnoresAppChart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dms-0.0.1-abc.tgz"), nil, 0o600))

	_, err := InfraChartTgz(dir, "dms")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dms-infra-*.tgz")
}
