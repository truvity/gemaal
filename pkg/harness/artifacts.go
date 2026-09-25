// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/truvity/gemaal/pkg/gemaalcfg"
)

// This file is the repo-agnostic half of a project's integration
// TestMain: the pieces url-shortener wrote locally (bar
// url-shortener/tests/harness_test.go) and every next adopter was about
// to copy verbatim. The VALUES file stays with the project — each
// chart's values contract is its own — but locating packaged charts,
// running the committed build hook, and the ring-pair install glue are
// identical across repos by construction, so they live here once.

// ResolveGitRoot returns the repository root of the current working
// directory, which is where gemaal.yaml, dist/ and the generated
// kubeconfig live.
func ResolveGitRoot(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("resolve git root: %w", err)
	}

	return strings.TrimSpace(string(out)), nil
}

// EnsureKubeconfig points kubectl/helm at the repo-root kubeconfig when
// KUBECONFIG is not already set. A repo that generates its kubeconfig
// (cfggen) keeps it at the root; a caller with their own KUBECONFIG
// keeps theirs.
func EnsureKubeconfig(gitRoot string) {
	if os.Getenv("KUBECONFIG") != "" {
		return
	}

	kubeconfigPath := filepath.Join(gitRoot, "kubeconfig")
	if _, err := os.Stat(kubeconfigPath); err == nil {
		_ = os.Setenv("KUBECONFIG", kubeconfigPath)
	}
}

// InfraChartTgz locates the packaged RING 2 chart alone, for callers that
// deploy infrastructure without the application: a suite that needs the
// project's database and none of its daemons.
//
// Separate from ChartTgzs rather than a flag on it, because the two
// answer different questions. ChartTgzs asks "is the ring pair complete"
// and an absent app chart is its error; here an absent app chart is the
// NORMAL state -- nothing built it, deliberately -- and demanding one
// would make the cheap path build every image it exists to avoid.
func InfraChartTgz(chartsDir, project string) (string, error) {
	entries, err := os.ReadDir(chartsDir)
	if err != nil {
		return "", fmt.Errorf("packaged charts not found: %w", err)
	}

	prefix := project + "-infra-"

	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".tgz") && strings.HasPrefix(name, prefix) {
			return filepath.Join(chartsDir, name), nil
		}
	}

	return "", fmt.Errorf("no packaged %s chart in %s", prefix+"*.tgz", chartsDir)
}

// AppChartTgz locates the packaged RING 3 chart alone, for the kind
// tier's DeployApp: the project's application chart with no ring2
// infrastructure pair required — kind's own fixture substitutes for the
// project's infra chart, which is never built or installed there.
//
// Mirrors InfraChartTgz's shape exactly, on the app side: ChartTgzs
// demands the complete pair, and an absent infra chart is its error;
// here an absent (or simply unbuilt) infra chart is the NORMAL state,
// and this must not fall into the "app-" prefix trap ChartTgzs's own
// tests guard — an infra tarball also matches the app prefix, so infra
// names are excluded explicitly rather than relying on match order.
func AppChartTgz(chartsDir, project string) (string, error) {
	entries, err := os.ReadDir(chartsDir)
	if err != nil {
		return "", fmt.Errorf("packaged charts not found: %w", err)
	}

	infraPrefix := project + "-infra-"
	prefix := project + "-"

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".tgz") || strings.HasPrefix(name, infraPrefix) {
			continue
		}

		if strings.HasPrefix(name, prefix) {
			return filepath.Join(chartsDir, name), nil
		}
	}

	return "", fmt.Errorf("no packaged %s chart in %s (excluding %s)", prefix+"*.tgz", chartsDir, infraPrefix+"*.tgz")
}

// ChartTgzs locates the packaged ring pair in chartsDir
// (dist/{project}/charts after the snapshot pipeline): the infra chart
// is {project}-infra-*.tgz, the app chart {project}-*.tgz. Errors state
// what is missing and nothing more — how the charts get there depends
// on GEMAAL_TEST_SKIP_BUILD, which only the caller knows.
func ChartTgzs(chartsDir, project string) (infraTgz, appTgz string, err error) {
	entries, err := os.ReadDir(chartsDir)
	if err != nil {
		return "", "", fmt.Errorf("packaged charts not found: %w", err)
	}

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".tgz") {
			continue
		}

		switch {
		case strings.HasPrefix(name, project+"-infra-"):
			infraTgz = filepath.Join(chartsDir, name)
		case strings.HasPrefix(name, project+"-"):
			appTgz = filepath.Join(chartsDir, name)
		}
	}

	if infraTgz == "" || appTgz == "" {
		return "", "", fmt.Errorf("packaged ring pair incomplete in %s (infra=%q app=%q)",
			chartsDir, infraTgz, appTgz)
	}

	return infraTgz, appTgz, nil
}

// BuildArtifacts runs the project's committed build hook from
// gemaal.yaml (the same command gemaalctl install --build runs, so
// tests and CLI cannot drift). The suite's Build phase should always
// run it unless GEMAAL_TEST_SKIP_BUILD says otherwise: build tooling's
// own input hashing makes an unchanged rebuild cheap, while skipping
// because tarballs merely EXIST would install stale charts.
func BuildArtifacts(ctx context.Context, cfg *gemaalcfg.Config, project, gitRoot string) error {
	hook := cfg.BuildHook(project)
	if len(hook) == 0 {
		return fmt.Errorf("gemaal.yaml carries no build hook for %s", project)
	}

	fmt.Fprintf(os.Stderr, "building artifacts: %s (%s=1 reuses dist/)\n",
		strings.Join(hook, " "), EnvSkipBuild)

	cmd := exec.CommandContext(ctx, hook[0], hook[1:]...)
	cmd.Dir = gitRoot
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// DeployInfra installs RING 2 ALONE: the project's databases, queues and
// claims, without the application that normally sits on them.
//
// The lane this exists for is a test suite that needs a real database
// (CNPG rather than a container) and never calls the application: with
// DeployPair it waits for every image to build and every daemon to roll
// out to reach a DSN. Uninstall is UninstallPair as usual -- it removes
// the app release too, and a release that was never installed is not an
// error.
func DeployInfra(
	ctx context.Context,
	cluster *Cluster,
	tenant Tenant,
	gitRoot, project string,
	valuesFiles, set []string,
) error {
	infraTgz, err := InfraChartTgz(filepath.Join(gitRoot, "dist", project, "charts"), project)
	if err != nil {
		return fmt.Errorf("%w — package it first (helmctl package --chart charts/%s-infra)", err, project)
	}

	return cluster.Install(ctx, tenant.Namespace, Install{
		Release:     InfraRelease(tenant.Release),
		Chart:       infraTgz,
		ValuesFiles: valuesFiles,
		Set:         set,
		Labels:      Labels{ExecutionID: DefaultExecutionID(time.Now())},
	})
}

// DeployPair installs the packaged ring pair from dist/{project}/charts
// into the standing tenant, ring2 first — ring3's pre-install hooks
// (migrations) need the infrastructure standing. The gemaal ledger is
// stamped on both release Secrets with a fresh execution id. valuesFiles
// and set pass through to BOTH releases.
func DeployPair(
	ctx context.Context,
	cluster *Cluster,
	tenant Tenant,
	gitRoot, project string,
	valuesFiles, set []string,
) error {
	infraTgz, appTgz, err := ChartTgzs(filepath.Join(gitRoot, "dist", project, "charts"), project)
	if err != nil {
		// Pointing at the snapshot task is useless advice when the
		// operator just disabled the build — name the actual contract.
		if skip, ferr := ParseSkipFlags(); ferr == nil && skip.Build {
			return fmt.Errorf("%w — %s skipped the build that fills that "+
				"directory, so the charts have to be there already: unset %s, "+
				"or run the %s build hook once by hand",
				err, EnvSkipBuild, EnvSkipBuild, project)
		}

		return fmt.Errorf("%w — run the %s build hook (gemaal.yaml hooks.build)", err, project)
	}

	return cluster.InstallPair(ctx, tenant, Pair{
		InfraChart:  infraTgz,
		AppChart:    appTgz,
		ValuesFiles: valuesFiles,
		Set:         set,
		Labels: Labels{
			ExecutionID: DefaultExecutionID(time.Now()),
		},
	})
}

// DeployApp installs RING 3 ALONE: the application chart, with no ring2
// infrastructure pair — the kind tier's lane, mirroring DeployInfra's
// ring2-alone counterpart on the other ring. The project's own
// infrastructure chart is never installed on kind; a fixture substitutes
// for it, so nothing here waits on or knows about one. Uninstall is the
// caller's own cluster.Uninstall(ctx, tenant.Namespace, tenant.Release)
// — there is no "-infra" companion release to also remove.
func DeployApp(
	ctx context.Context,
	cluster *Cluster,
	tenant Tenant,
	gitRoot, project string,
	valuesFiles, set []string,
) error {
	appTgz, err := AppChartTgz(filepath.Join(gitRoot, "dist", project, "charts"), project)
	if err != nil {
		return fmt.Errorf("%w — package it first (helmctl package --chart charts/%s)", err, project)
	}

	return cluster.Install(ctx, tenant.Namespace, Install{
		Release:     tenant.Release,
		Chart:       appTgz,
		ValuesFiles: valuesFiles,
		Set:         set,
		Labels:      Labels{ExecutionID: DefaultExecutionID(time.Now())},
	})
}
