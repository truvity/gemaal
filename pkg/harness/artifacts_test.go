// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package harness

import (
	"context"
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

// The kind tier's ring3-alone lane's whole point: an infra chart that
// was never built is the NORMAL state here, not an incomplete pair.
// ChartTgzs rejects this same directory, and must keep doing so.
func TestAppChartTgzWithoutInfraChart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dms-0.0.1-abc.tgz"), nil, 0o600))

	app, err := AppChartTgz(dir, "dms")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "dms-0.0.1-abc.tgz"), app)

	_, _, pairErr := ChartTgzs(dir, "dms")
	require.Error(t, pairErr, "ChartTgzs must still refuse a half-built pair")
}

// The infra chart must never be mistaken for the app one: "dms-" is a
// prefix of "dms-infra-", so a careless match returns the ring2 chart
// and the kind lane installs the infrastructure it exists to skip.
func TestAppChartTgzIgnoresInfraChart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dms-infra-0.0.1-abc.tgz"), nil, 0o600))

	_, err := AppChartTgz(dir, "dms")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dms-*.tgz")
}

func TestAppChartTgzMissingDir(t *testing.T) {
	t.Parallel()

	_, err := AppChartTgz(filepath.Join(t.TempDir(), "nope"), "dms")
	require.ErrorContains(t, err, "not found")
}

func TestDeployApp(t *testing.T) {
	t.Run("installs the app release alone from the packaged app chart", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "dms-0.0.1-abc.tgz"), nil, 0o600))

		gitRoot := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(gitRoot, "dist", "dms", "charts"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(gitRoot, "dist", "dms", "charts", "dms-0.0.1-abc.tgz"), nil, 0o600))

		s := &stubRunner{}
		c := &Cluster{Runner: s}
		tenant := Tenant{Namespace: "ci-kind-suite", Release: "myapp"}

		err := DeployApp(context.Background(), c, tenant, gitRoot, "dms", []string{"v.yaml"}, []string{"k=v"})
		require.NoError(t, err)

		require.Len(t, s.calls, 1)
		joined := s.joined()[0]
		assert.Contains(t, joined, "--install myapp "+filepath.Join(gitRoot, "dist", "dms", "charts", "dms-0.0.1-abc.tgz"),
			"the release is named after the tenant, no -infra suffix")
		assert.Contains(t, joined, "--namespace ci-kind-suite")
		assert.Contains(t, joined, "--values v.yaml")
		assert.Contains(t, joined, "--set k=v")
	})

	t.Run("a missing app chart names the packaging step, not the build hook", func(t *testing.T) {
		gitRoot := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(gitRoot, "dist", "dms", "charts"), 0o755))

		s := &stubRunner{}
		c := &Cluster{Runner: s}
		tenant := Tenant{Namespace: "ci-kind-suite", Release: "myapp"}

		err := DeployApp(context.Background(), c, tenant, gitRoot, "dms", nil, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "helmctl package")
		assert.Empty(t, s.calls, "no helm install when the chart is missing")
	})
}
