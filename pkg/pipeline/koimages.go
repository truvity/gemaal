package pipeline

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// publishKoImages builds and pushes every `kos` entry of the project's
// .goreleaser.yaml, the way publishImages does it for `dockers_v2`.
//
// It exists because the dev loop moved off goreleaser-pro. The Pro-only
// `--nightly` bought exactly two things: a build from a dirty tree, which
// OSS gives as `--skip=validate`, and a ko publish. `--snapshot` does not
// publish — it loads into the local daemon — so a snapshot build would
// leave the digest-pinned charts referencing images that were never
// pushed. This step is the push.
//
// ko is driven as a command rather than linked as a library, which is
// how every other tool in this pipeline is driven: goreleaser, helm,
// helmctl and docker are all argv vectors in `commands:`. ko's own build
// is what this needs — cross-compiled per platform, one push of one
// index, no daemon and no QEMU — and it needs none of the control that
// linking would buy.
//
// One push per image, not two. ko assembles the whole multi-platform
// index locally and pushes it once, so the tag is written once. That is
// the property publishImages has to reconstruct by hand for buildx, and
// the reason an immutable repository accepts what ko sends it.
//
// The .goreleaser.yaml stays the single source of truth: repositories,
// tags, platforms, base image, labels and the naming strategy are read
// from the `kos` entries and rendered with goreleaser's own template
// context, and `build:` resolves main, flags, ldflags and env from the
// `builds` entry it names — exactly as goreleaser resolves them. So a
// laptop `goreleaser release` and a flow build the same images.
func (p *Pipeline) publishKoImages(ctx context.Context, env []string, snapshot bool) ([]publishedImage, error) {
	gcfg, err := loadGoreleaserConfig(p.abs(p.cfg.GoreleaserConfig))
	if err != nil {
		return nil, err
	}

	if len(gcfg.Kos) == 0 {
		return nil, nil
	}

	meta, err := loadGoreleaserMetadata(p.abs(p.cfg.DistDir))
	if err != nil {
		return nil, err
	}

	data := newTemplateData(meta, snapshot, append(os.Environ(), env...))
	projectDir := gcfg.projectDir(p.root)

	published := make([]publishedImage, 0, len(gcfg.Kos))

	// Serial, unlike the buildx step. ko cross-compiles every platform in
	// one process and the work is CPU-bound Go builds, so running the
	// entries concurrently would contend rather than overlap.
	for i := range gcfg.Kos {
		img, err := p.publishKoImage(ctx, env, &gcfg.Kos[i], gcfg, data, projectDir)
		if err != nil {
			return nil, err
		}

		if img == nil {
			continue
		}

		published = append(published, *img)
	}

	if err := p.appendArtifacts(published); err != nil {
		return nil, err
	}

	return published, nil
}

func (p *Pipeline) publishKoImage(
	ctx context.Context,
	env []string,
	k *koEntry,
	gcfg *goreleaserConfig,
	data templateData,
	projectDir string,
) (*publishedImage, error) {
	if k.Disable != "" {
		v, err := data.render(k.Disable)
		if err != nil {
			return nil, err
		}

		if v == "true" {
			return nil, nil
		}
	}

	build, err := gcfg.findBuild(k)
	if err != nil {
		return nil, err
	}

	// Tags render to "" when their condition is false — that is how a
	// config suppresses `latest` on a dev build — and an empty tag is
	// dropped rather than pushed as a tag named "".
	tags := make([]string, 0, len(k.Tags))

	for _, t := range k.Tags {
		v, err := data.render(t)
		if err != nil {
			return nil, fmt.Errorf("ko %s: render tag %q: %w", k.ID, t, err)
		}

		if v = strings.TrimSpace(v); v != "" {
			tags = append(tags, v)
		}
	}

	if len(tags) == 0 {
		return nil, fmt.Errorf("ko %s: every tag rendered empty, so nothing would be published", k.ID)
	}

	platforms := k.Platforms
	if len(platforms) == 0 {
		platforms = []string{"linux/amd64", "linux/arm64"}
	}

	main := k.Main
	if main == "" {
		main = build.Main
	}

	if main == "" {
		return nil, fmt.Errorf("ko %s: no main package, and the build it names declares none", k.ID)
	}

	repo, err := p.koRepository(k, data, env)
	if err != nil {
		return nil, err
	}

	// ko names the image from the repository and ONE of three strategies.
	//
	// THE ORDER IS KO'S, and it is not the order the fields are written
	// in: preserve_import_paths, then base_import_paths, then bare
	// (ko's options.MakeNamer). It matters because the estate's configs
	// set `bare: true` AND `base_import_paths: true` on the same entry,
	// and ko resolves that to base_import_paths. Reading `bare` first
	// names the image after the repository's PARENT — which a registry
	// answers with a 403, since the role may push to `dms/wallet` and
	// not to `dms`.
	image := repo

	switch {
	case k.PreserveImportPaths:
		image = repo + "/" + strings.TrimPrefix(strings.TrimPrefix(main, "."), "/")
	case k.BaseImportPaths:
		image = repo + "/" + path.Base(strings.TrimSuffix(main, "/"))
	case k.Bare:
	default:
		image = repo + "/" + path.Base(strings.TrimSuffix(main, "/"))
	}

	// ko writes the reference it pushed, with the digest, to this file.
	// Reading it rather than parsing stdout is what makes the digest the
	// registry's answer instead of this code's guess.
	refs, err := os.CreateTemp("", "gemaal-ko-refs-*")
	if err != nil {
		return nil, fmt.Errorf("ko %s: %w", k.ID, err)
	}

	refsPath := refs.Name()

	_ = refs.Close()

	defer func() { _ = os.Remove(refsPath) }()

	args := []string{
		"build",
		"--push",
		"--platform", strings.Join(platforms, ","),
		"--tags", strings.Join(tags, ","),
		"--image-refs", refsPath,
	}

	// Exactly ONE naming flag, chosen with ko's precedence above. Passing
	// two would leave the choice to ko and make the name computed here a
	// guess about which one it picks.
	switch {
	case k.PreserveImportPaths:
		args = append(args, "--preserve-import-paths")
	case k.BaseImportPaths:
		args = append(args, "--base-import-paths")
	case k.Bare:
		args = append(args, "--bare")
	}

	if k.SBOM != "" {
		args = append(args, "--sbom", k.SBOM)
	}

	for _, l := range sortedPairs(k.Labels) {
		rendered, err := data.render(l)
		if err != nil {
			return nil, fmt.Errorf("ko %s: render label %q: %w", k.ID, l, err)
		}

		args = append(args, "--image-label", rendered)
	}

	for _, a := range sortedPairs(k.Annotations) {
		rendered, err := data.render(a)
		if err != nil {
			return nil, fmt.Errorf("ko %s: render annotation %q: %w", k.ID, a, err)
		}

		args = append(args, "--image-annotation", rendered)
	}

	args = append(args, main)

	// The build's own environment, plus the repository ko publishes to
	// and the base image the entry names. ko reads both from the
	// environment, which keeps this free of a generated .ko.yaml that
	// would then be a second source of truth.
	koEnv := append([]string{}, env...)
	koEnv = append(koEnv, "KO_DOCKER_REPO="+repo)

	if k.BaseImage != "" {
		base, err := data.render(k.BaseImage)
		if err != nil {
			return nil, fmt.Errorf("ko %s: render base_image: %w", k.ID, err)
		}

		koEnv = append(koEnv, "KO_DEFAULTBASEIMAGE="+base)
	}

	if flags := koGoFlags(k, build, data); len(flags) > 0 {
		rendered := make([]string, 0, len(flags))

		for _, f := range flags {
			v, err := data.render(f)
			if err != nil {
				return nil, fmt.Errorf("ko %s: render flag %q: %w", k.ID, f, err)
			}

			rendered = append(rendered, v)
		}

		koEnv = append(koEnv, "GOFLAGS="+strings.Join(rendered, " "))
	}

	if ldflags := koLdflags(k, build); len(ldflags) > 0 {
		rendered := make([]string, 0, len(ldflags))

		for _, f := range ldflags {
			v, err := data.render(f)
			if err != nil {
				return nil, fmt.Errorf("ko %s: render ldflags %q: %w", k.ID, f, err)
			}

			rendered = append(rendered, v)
		}

		koEnv = append(koEnv, "GOLDFLAGS="+strings.Join(rendered, " "))
	}

	for _, e := range append(append([]string{}, build.Env...), k.Env...) {
		v, err := data.render(e)
		if err != nil {
			return nil, fmt.Errorf("ko %s: render env %q: %w", k.ID, e, err)
		}

		koEnv = append(koEnv, v)
	}

	workdir := k.WorkingDir
	if workdir == "" {
		workdir = build.Dir
	}

	if workdir == "" {
		workdir = projectDir
	} else if !filepath.IsAbs(workdir) {
		workdir = filepath.Join(p.root, workdir)
	}

	if err := p.runIn(ctx, workdir, koEnv, p.cfg.Commands.Ko, args...); err != nil {
		return nil, fmt.Errorf("ko %s: %w", k.ID, err)
	}

	digest, err := readKoDigest(refsPath)
	if err != nil {
		return nil, fmt.Errorf("ko %s: %w", k.ID, err)
	}

	return &publishedImage{
		ID:        k.ID,
		Image:     image,
		Tags:      tags,
		Digest:    digest,
		Platforms: platforms,
	}, nil
}

// koRepository is where ko pushes. An entry's `repositories` wins; with
// none, KO_DOCKER_REPO from the flow's destination environment does,
// which is what every project in this estate relies on.
func (p *Pipeline) koRepository(k *koEntry, data templateData, env []string) (string, error) {
	if len(k.Repositories) > 0 {
		v, err := data.render(k.Repositories[0])
		if err != nil {
			return "", fmt.Errorf("ko %s: render repository: %w", k.ID, err)
		}

		if v != "" {
			return v, nil
		}
	}

	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], "KO_DOCKER_REPO="); ok && v != "" {
			return v, nil
		}
	}

	return "", fmt.Errorf("ko %s: no repository, and KO_DOCKER_REPO is not set", k.ID)
}

// findBuild resolves the `builds` entry a ko entry names. An entry that
// names one that does not exist is refused here rather than silently
// built from goreleaser's defaults, which would compile the wrong main.
func (g *goreleaserConfig) findBuild(k *koEntry) (goreleaserBuild, error) {
	if k.Build == "" {
		return goreleaserBuild{}, nil
	}

	for _, b := range g.Builds {
		if b.ID == k.Build {
			return b, nil
		}
	}

	return goreleaserBuild{}, fmt.Errorf("ko %s: names build %q, which this config does not declare", k.ID, k.Build)
}

func koGoFlags(k *koEntry, build goreleaserBuild, _ templateData) []string {
	if len(k.Flags) > 0 {
		return k.Flags
	}

	return build.Flags
}

func koLdflags(k *koEntry, build goreleaserBuild) []string {
	if len(k.Ldflags) > 0 {
		return k.Ldflags
	}

	return build.Ldflags
}

// sortedPairs renders a map as sorted "k=v" strings, so that two runs of
// the same configuration produce the same argv and the same image.
func sortedPairs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}

	sort.Strings(out)

	return out
}

// readKoDigest reads what ko recorded in --image-refs. ko writes one
// fully-qualified reference per line, `repo@sha256:...`, and the digest
// is the registry's answer rather than anything computed here.
func readKoDigest(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the references ko pushed: %w", err)
	}

	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		_, digest, ok := strings.Cut(line, "@")
		if !ok {
			return "", fmt.Errorf("ko wrote %q, which carries no digest", line)
		}

		return digest, nil
	}

	return "", fmt.Errorf("ko pushed nothing: it wrote no reference")
}
