// Package pipeline holds the gemaalctl artifact pipeline: the dev-loop
// snapshot build, the preview chart push, and the gated stable release.
//
// The flows are a Go port of bar's url-shortener release scripts
// (url-shortener/scripts/{release-env,charts-lock,charts-select,
// artifacts-snapshot,charts-push,release-stable}.sh — eight review
// rounds on bar#487 made those scripts the spec). Every hardening in
// them survives the port:
//
//   - Two destinations, one entrypoint each: snapshot and push-preview
//     are wired to the preview registry, release-stable to the stable
//     one; no runtime knob selects a destination. The registry is both a
//     build-time input and a push-time destination (packaging bakes
//     digest-pinned, registry-qualified image refs into the ring3
//     chart), so each build stamps .release-type provenance and the
//     preview push refuses charts packaged for the other registry.
//   - The stable release verifies five gates — clean tree, HEAD ==
//     origin/<releaseBranch>, a <tagPrefix>v* tag on HEAD, that tag
//     pushed and matching origin, and a CI-provenance warning — ALL
//     before any build step or credential use. There is deliberately no
//     escape hatch: gates that can be skipped aren't gates.
//   - Ring-pair selection accepts exactly one tarball per ring, both
//     rings on the same version, and re-reads each tarball's own
//     Chart.yaml so a file merely named like a ring tarball cannot be
//     published under an identity it does not carry.
//   - Push flows freeze the shared charts directory into a private
//     staging dir under the chart lock, validate the STAGE, and publish
//     from it — validate-then-copy from the shared dir would be a TOCTOU
//     hole.
//   - One exclusive file lock (github.com/gofrs/flock) spans each
//     writer's entire mutation window; the lockfile lives outside every
//     cleaned path so `goreleaser --clean` never deletes its inode.
//   - Snapshot builds with `goreleaser release --nightly` (publishes
//     from a dirty tree; --snapshot cannot substitute — it skips
//     publishing); the stable release runs full goreleaser validation.
//   - A failed stable release reports what landed, what MIGHT have
//     landed (started-but-unconfirmed pushes are UNKNOWN, not absent)
//     and the failure-point-specific recovery, truthful about the
//     IMMUTABLE_WITH_EXCLUSION stable repositories where "just re-run
//     the same tag" does not exist.
package pipeline

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Pipeline executes the three flows for one project configuration.
type Pipeline struct {
	cfg    *Config
	runner Runner
	log    *slog.Logger
	// stderr carries the operator-facing prose the scripts wrote to fd 2:
	// gate failures, refusals, the CI-provenance warning and the
	// partial-publish report. Progress goes through the slog logger.
	stderr io.Writer
	// root is the git toplevel, resolved by each flow before anything else.
	root string
	// ambient drops every AWS profile the config names (no --profile,
	// no AWS_PROFILE) so credentials resolve through the default chain
	// — env, container, IMDS. This is the CI face: runners hold
	// per-phase OIDC role creds in env, and the CLI would IGNORE them
	// the moment an explicit --profile appears. Laptops keep their
	// committed profiles; GEMAAL_AWS_AMBIENT flips the same
	// pipeline.yaml to ambient without a config fork (GEMAAL_* env
	// always wins — gemaal.yaml doctrine).
	ambient bool
}

// New wires a Pipeline. runner executes every external command; stderr
// receives operator guidance (pass os.Stderr in the CLI).
func New(cfg *Config, runner Runner, log *slog.Logger, stderr io.Writer) *Pipeline {
	ambient := false
	switch os.Getenv("GEMAAL_AWS_AMBIENT") {
	case "1", "true", "yes":
		ambient = true
	}

	return &Pipeline{cfg: cfg, runner: runner, log: log, stderr: stderr, ambient: ambient}
}

// errf writes operator-facing prose to stderr, mirroring the scripts'
// `echo ... >&2` blocks.
func (p *Pipeline) errf(format string, args ...any) {
	_, _ = fmt.Fprintf(p.stderr, format, args...)
}

// abs turns a repo-relative slash path into an absolute host path.
func (p *Pipeline) abs(rel string) string {
	return filepath.Join(p.root, filepath.FromSlash(rel))
}

func (p *Pipeline) run(ctx context.Context, env, base []string, args ...string) error {
	argv := make([]string, 0, len(base)+len(args))
	argv = append(append(argv, base...), args...)

	return p.runner.Run(ctx, Command{Argv: argv, Dir: p.root, Env: env})
}

func (p *Pipeline) output(ctx context.Context, env, base []string, args ...string) (string, error) {
	argv := make([]string, 0, len(base)+len(args))
	argv = append(append(argv, base...), args...)

	return p.runner.Output(ctx, Command{Argv: argv, Dir: p.root, Env: env})
}

// resolveRoot locates the git toplevel; every flow runs from there (the
// scripts cd to `git rev-parse --show-toplevel` first — monorepo paths
// are root-relative).
func (p *Pipeline) resolveRoot(ctx context.Context) error {
	out, err := p.runner.Output(ctx, Command{Argv: []string{"git", "rev-parse", "--show-toplevel"}})
	if err != nil {
		return fmt.Errorf("resolve git root: %w", err)
	}

	p.root = strings.TrimSpace(out)
	if p.root == "" {
		return fmt.Errorf("resolve git root: empty toplevel")
	}

	return nil
}

// destinationEnv builds the exported environment the scripts set once
// per workflow: REGISTRY, AWS_PROFILE, AWS_REGION and — for builds —
// KO_DOCKER_REPO. Attached to every command in a flow.
func (p *Pipeline) destinationEnv(dest Destination, forBuild bool) []string {
	env := []string{
		"REGISTRY=" + dest.Registry,
		"AWS_REGION=" + p.cfg.AWSRegion,
	}
	if !p.ambient {
		env = append(env, "AWS_PROFILE="+dest.Profile)
	}
	if forBuild {
		env = append(env, "KO_DOCKER_REPO="+dest.Registry+"/"+p.cfg.Project)
	}

	return env
}

// profileArgs renders the --profile pair for aws/helmctl invocations —
// empty under ambient credentials, where the default chain must see no
// profile at all (an explicit --profile makes the CLI ignore env creds).
func (p *Pipeline) profileArgs(dest Destination) []string {
	if p.ambient {
		return nil
	}

	return []string{"--profile", dest.Profile}
}

// cleanChartsOutput invalidates the previous build's output BEFORE the
// first build step: charts and stamp are dropped together and only
// rewritten by a run that reaches the stamp write, so a build that dies
// mid-way leaves nothing a push flow would publish as if this build had
// produced it. Callers hold the chart lock — this rm opens the mutation
// window.
func (p *Pipeline) cleanChartsOutput() error {
	matches, err := filepath.Glob(filepath.Join(p.abs(p.cfg.ChartsOut()), "*.tgz"))
	if err != nil {
		return fmt.Errorf("clean charts output: %w", err)
	}

	for _, m := range matches {
		if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clean charts output: %w", err)
		}
	}

	if err := os.Remove(p.abs(p.cfg.StampPath())); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clean charts output: %w", err)
	}

	return nil
}

// manifestVersion extracts the release version from the generated
// manifest — the first top-level `version:` scalar (the scripts' awk
// '/^version:/ {print $2; exit}').
func (p *Pipeline) manifestVersion() (string, error) {
	raw, err := os.ReadFile(p.abs(p.cfg.ManifestPath()))
	if err != nil {
		return "", fmt.Errorf("read manifest: %w", err)
	}

	for _, line := range strings.Split(string(raw), "\n") {
		rest, ok := strings.CutPrefix(line, "version:")
		if !ok {
			continue
		}

		if fields := strings.Fields(rest); len(fields) > 0 {
			return fields[0], nil
		}

		break
	}

	p.errf("error: no version in %s\n", p.cfg.ManifestPath())

	return "", fmt.Errorf("no version in %s", p.cfg.ManifestPath())
}

// packageCharts packages ring2 (vendoring its dependencies first when
// configured — the helmctl CLI has no --vendor-dependencies flag yet)
// with the build's version, then ring3 with the digest-pinned manifest.
func (p *Pipeline) packageCharts(ctx context.Context, env []string, version string) error {
	if err := os.MkdirAll(p.abs(p.cfg.ChartsOut()), 0o755); err != nil {
		return fmt.Errorf("create charts output dir: %w", err)
	}

	if p.cfg.Charts.Ring2.VendorDependencies {
		if err := p.run(ctx, env, p.cfg.Commands.Helm, "dependency", "update", p.cfg.Charts.Ring2.Path); err != nil {
			return err
		}
	}

	if err := p.run(ctx, env, p.cfg.Commands.Helmctl,
		"package",
		"--chart", p.cfg.Charts.Ring2.Path,
		"--version", version,
		"--output", p.cfg.ChartsOut(),
	); err != nil {
		return err
	}

	return p.run(ctx, env, p.cfg.Commands.Helmctl,
		"package",
		"--chart", p.cfg.Charts.Ring3.Path,
		"--manifest", p.cfg.ManifestPath(),
		"--require-image-digests",
		"--output", p.cfg.ChartsOut(),
	)
}

// writeStamp completes a build's chart output: which registry the chart
// values point at. The stamp is the last write of the mutation window.
func (p *Pipeline) writeStamp(value string) error {
	if err := os.WriteFile(p.abs(p.cfg.StampPath()), []byte(value+"\n"), 0o644); err != nil {
		return fmt.Errorf("write release-type stamp: %w", err)
	}

	return nil
}

// readStamp reads a .release-type stamp, whitespace stripped (the
// scripts' `tr -d '[:space:]'`). ok is false when the stamp is absent.
func readStamp(path string) (value string, ok bool, err error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("read release-type stamp: %w", err)
	}

	return strings.Join(strings.Fields(string(raw)), ""), true, nil
}

// stageCharts freezes the shared charts output into a private temp dir:
// a BLIND copy of the ENTIRE directory (tarballs + stamp, dotfiles
// included) taken under the chart lock. Validate-then-copy from the
// shared dir would be a TOCTOU hole — every check after this runs
// against the immutable snapshot, and pushes publish from it.
func (p *Pipeline) stageCharts() (string, error) {
	stage, err := os.MkdirTemp("", "gemaal-charts-stage-")
	if err != nil {
		return "", fmt.Errorf("create staging dir: %w", err)
	}

	if err := copyTree(p.abs(p.cfg.ChartsOut()), stage); err != nil {
		_ = os.RemoveAll(stage)

		return "", fmt.Errorf("stage charts output: %w", err)
	}

	return stage, nil
}

// copyTree copies a directory's regular files and subdirectories into
// dst (which must exist). Non-regular files are refused — nothing in a
// packaged charts directory is legitimately a device, socket or symlink.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		if rel == "." {
			return nil
		}

		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		if !d.Type().IsRegular() {
			return fmt.Errorf("refusing to stage non-regular file %s", path)
		}

		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()

		return err
	}

	return out.Close()
}

// ecrLogin performs `aws ecr get-login-password | helm registry login
// --password-stdin`. Every validation runs before this: a bad chart
// directory never reaches a credential.
func (p *Pipeline) ecrLogin(ctx context.Context, env []string, dest Destination) error {
	args := []string{"ecr", "get-login-password"}
	args = append(args, p.profileArgs(dest)...)
	args = append(args, "--region", p.cfg.AWSRegion)

	password, err := p.output(ctx, env, p.cfg.Commands.AWS, args...)
	if err != nil {
		return err
	}

	argv := make([]string, 0, len(p.cfg.Commands.Helm)+6)
	argv = append(argv, p.cfg.Commands.Helm...)
	argv = append(argv, "registry", "login", "--username", "AWS", "--password-stdin", dest.Registry)

	return p.runner.Run(ctx, Command{Argv: argv, Dir: p.root, Env: env, Stdin: strings.NewReader(password)})
}

// pushChart publishes one staged tarball via helmctl push.
func (p *Pipeline) pushChart(ctx context.Context, env []string, dest Destination, name, tgz, version string) error {
	args := []string{
		"push",
		"--tgz", tgz,
		"--registry", dest.Registry,
		"--repository", p.cfg.ChartRepository(name),
	}
	args = append(args, p.profileArgs(dest)...)
	args = append(args,
		"--name", name,
		"--version", version,
	)

	return p.run(ctx, env, p.cfg.Commands.Helmctl, args...)
}

// helmctlDisplay renders the configured helmctl command for recovery
// advice (e.g. "go tool helmctl").
func (p *Pipeline) helmctlDisplay() string { return strings.Join(p.cfg.Commands.Helmctl, " ") }

// helmDisplay renders the configured helm command for recovery advice.
func (p *Pipeline) helmDisplay() string { return strings.Join(p.cfg.Commands.Helm, " ") }
