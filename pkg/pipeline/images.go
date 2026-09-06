package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"
)

// publishImages builds and publishes every dockers_v2 image of the
// project's .goreleaser.yaml — goreleaser itself runs with
// --skip=docker in both flows. The reason is one registry property:
//
// A multi-node buildx builder (the CI plane's per-arch BuildKit pair,
// registered as one builder so each platform builds natively) pushes
// the second node's platform image UNDER THE TAG and then writes the
// merged manifest list under the same tag. Two writes, one tag. The
// preview registry is mutable and never noticed; every stable
// repository is IMMUTABLE and refuses the second write, which burned
// dms v0.33.0 (truvity/dms#13 moved that release to a hosted runner
// with one QEMU node to get around it). goreleaser's dockers_v2 drives
// exactly that `docker buildx build --push` and offers no other shape.
//
// This step drives the shape buildx should have used:
//
//  1. one `docker buildx build` PER PLATFORM, exported with
//     push-by-digest — a digest push is not a tag write, so an immutable
//     repository accepts it, and the two platforms run in parallel on
//     the two builders;
//  2. after EVERY image of the release has its digests, one
//     `docker buildx imagetools create` per image writes the manifest
//     list under the tags — the only tag write there is, and the last
//     step, so a failure anywhere earlier leaves untagged blobs and no
//     burned version;
//  3. the results are appended to goreleaser's artifacts.json as the
//     "Docker Image" entries goreleaser would have written, so helmctl's
//     goreleaser-manifest pins the charts exactly as before.
//
// The .goreleaser.yaml stays the single source of truth: images, tags,
// flags, build_args, labels, extra_files and platforms are read from
// its dockers_v2 entries and rendered with goreleaser's template
// context (dist/metadata.json supplies the version goreleaser computed),
// so a laptop `goreleaser release --snapshot` and a flow build the same
// images. The build context is assembled the way goreleaser assembles
// it: the Dockerfile at the context root, extra_files at their
// project-relative paths (paths resolve against monorepo.dir). Binaries
// (`ids`) are not staged — every image in this estate is either ko
// (goreleaser's, untouched by this step) or a Node bundle via
// extra_files — and an entry that asks for them is refused loudly rather
// than built wrong.
func (p *Pipeline) publishImages(ctx context.Context, env []string, nightly bool) ([]publishedImage, error) {
	gcfg, err := loadGoreleaserConfig(p.abs(p.cfg.GoreleaserConfig))
	if err != nil {
		return nil, err
	}

	if len(gcfg.DockersV2) == 0 {
		return nil, nil
	}

	meta, err := loadGoreleaserMetadata(p.abs(p.cfg.DistDir))
	if err != nil {
		return nil, err
	}

	data := newTemplateData(meta, nightly, append(os.Environ(), env...))
	projectDir := gcfg.projectDir(p.root)

	// Phase 1: every platform of every image, by digest.
	plans := make([]*imagePlan, 0, len(gcfg.DockersV2))
	for i := range gcfg.DockersV2 {
		plan, err := p.planImage(&gcfg.DockersV2[i], data, projectDir)
		if err != nil {
			return nil, err
		}

		if plan == nil {
			continue
		}

		plans = append(plans, plan)
	}

	for _, plan := range plans {
		if err := p.buildPlatforms(ctx, env, plan); err != nil {
			return nil, err
		}
	}

	// Phase 2: the tag writes — one per image, after all builds landed.
	published := make([]publishedImage, 0, len(plans))
	for _, plan := range plans {
		img, err := p.tagImage(ctx, env, plan)
		if err != nil {
			return published, err
		}

		published = append(published, img)
	}

	if err := p.appendArtifacts(published); err != nil {
		return published, err
	}

	return published, nil
}

// imagePlan is one dockers_v2 entry, rendered and ready to build.
type imagePlan struct {
	id        string
	image     string // repository, no tag
	tags      []string
	platforms []string
	flags     []string
	buildArgs []string // k=v, sorted
	labels    []string // k=v, sorted
	sbom      bool
	context   string            // absolute path of the assembled build context
	digests   map[string]string // platform → pushed digest
}

// publishedImage is the outcome of one entry: the manifest-list digest
// its tags now point at.
type publishedImage struct {
	ID        string
	Image     string
	Tags      []string
	Digest    string
	Platforms []string
}

func (p *Pipeline) planImage(d *dockerV2, data templateData, projectDir string) (*imagePlan, error) {
	if d.Disable != "" {
		v, err := data.render(d.Disable)
		if err != nil {
			return nil, err
		}

		if strings.EqualFold(strings.TrimSpace(v), "true") {
			p.log.Info("image disabled by its dockers_v2 entry", slog.String("id", d.ID))

			return nil, nil
		}
	}

	if len(d.IDs) > 0 {
		return nil, fmt.Errorf("dockers_v2 %q: `ids` (goreleaser binaries in the build context) is not supported "+
			"by the image step; ship the binary via ko or extra_files", d.ID)
	}

	images, err := data.renderAll(d.Images)
	if err != nil {
		return nil, fmt.Errorf("dockers_v2 %q images: %w", d.ID, err)
	}

	if len(images) != 1 {
		return nil, fmt.Errorf("dockers_v2 %q: exactly one image repository is supported, got %d", d.ID, len(images))
	}

	tags, err := data.renderAll(d.Tags)
	if err != nil {
		return nil, fmt.Errorf("dockers_v2 %q tags: %w", d.ID, err)
	}

	if len(tags) == 0 {
		return nil, fmt.Errorf("dockers_v2 %q: no tag renders non-empty", d.ID)
	}

	flags, err := data.renderAll(d.Flags)
	if err != nil {
		return nil, fmt.Errorf("dockers_v2 %q flags: %w", d.ID, err)
	}

	buildArgs, err := renderMap(data, d.BuildArgs)
	if err != nil {
		return nil, fmt.Errorf("dockers_v2 %q build_args: %w", d.ID, err)
	}

	labels, err := renderMap(data, d.Labels)
	if err != nil {
		return nil, fmt.Errorf("dockers_v2 %q labels: %w", d.ID, err)
	}

	ctxDir, err := p.assembleContext(d, projectDir)
	if err != nil {
		return nil, err
	}

	sbom := true
	switch strings.ToLower(strings.TrimSpace(d.SBOM)) {
	case "none", "false", "disabled":
		sbom = false
	}

	return &imagePlan{
		id:        d.ID,
		image:     images[0],
		tags:      tags,
		platforms: d.Platforms,
		flags:     flags,
		buildArgs: buildArgs,
		labels:    labels,
		sbom:      sbom,
		context:   ctxDir,
		digests:   map[string]string{},
	}, nil
}

func renderMap(data templateData, m map[string]string) ([]string, error) {
	out := make([]string, 0, len(m))
	for k, v := range m {
		r, err := data.render(v)
		if err != nil {
			return nil, err
		}

		out = append(out, k+"="+r)
	}

	sort.Strings(out)

	return out, nil
}

// assembleContext stages the entry's build context under the dist
// directory: Dockerfile at the root, extra_files at their relative
// paths. Under dist because goreleaser --clean wipes it at the start of
// every build, so a context never outlives its run.
func (p *Pipeline) assembleContext(d *dockerV2, projectDir string) (string, error) {
	id := d.ID
	if id == "" {
		id = "image"
	}

	ctxDir := filepath.Join(p.abs(p.cfg.DistDir), "docker", id)
	if err := os.RemoveAll(ctxDir); err != nil {
		return "", fmt.Errorf("dockers_v2 %q: reset context: %w", d.ID, err)
	}

	if err := os.MkdirAll(ctxDir, 0o755); err != nil {
		return "", fmt.Errorf("dockers_v2 %q: create context: %w", d.ID, err)
	}

	src := filepath.Join(projectDir, filepath.FromSlash(d.Dockerfile))
	if err := copyPath(src, filepath.Join(ctxDir, "Dockerfile")); err != nil {
		return "", fmt.Errorf("dockers_v2 %q: dockerfile %s: %w", d.ID, d.Dockerfile, err)
	}

	for _, ef := range d.ExtraFiles {
		rel := filepath.FromSlash(strings.TrimSuffix(ef, "/"))
		if rel == "" || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			return "", fmt.Errorf("dockers_v2 %q: extra_files entry %q must be a relative path inside the project", d.ID, ef)
		}

		if err := copyPath(filepath.Join(projectDir, rel), filepath.Join(ctxDir, rel)); err != nil {
			return "", fmt.Errorf("dockers_v2 %q: extra_files %s: %w", d.ID, ef, err)
		}
	}

	return ctxDir, nil
}

// copyPath copies a file or a directory tree, following the source's
// modes. Symlinks are copied as the file they point at — the way a
// tar-based context would see them.
func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		return copyFileWithMode(src, dst, info.Mode())
	}

	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		fi, err := os.Stat(path)
		if err != nil {
			return err
		}

		return copyFileWithMode(path, target, fi.Mode())
	})
}

func copyFileWithMode(src, dst string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()

		return err
	}

	return out.Close()
}

// buildPlatforms runs one buildx build per platform, concurrently — on
// a two-node builder that is one build per node — each exported
// push-by-digest to the image repository. The digest comes back through
// --metadata-file (containerimage.digest).
func (p *Pipeline) buildPlatforms(ctx context.Context, env []string, plan *imagePlan) error {
	type result struct {
		platform, digest string
	}

	results := make([]result, len(plan.platforms))
	eg, ctx := errgroup.WithContext(ctx)

	for i, platform := range plan.platforms {
		eg.Go(func() error {
			metaFile := filepath.Join(plan.context, "..", plan.id+"-"+strings.ReplaceAll(platform, "/", "-")+".json")

			args := []string{
				"buildx", "build",
				"--platform", platform,
				"--file", filepath.Join(plan.context, "Dockerfile"),
				"--output", "type=image,name=" + plan.image + ",push=true,push-by-digest=true,oci-mediatypes=true",
				"--metadata-file", metaFile,
			}
			for _, kv := range plan.buildArgs {
				args = append(args, "--build-arg", kv)
			}
			for _, kv := range plan.labels {
				args = append(args, "--label", kv)
			}
			if plan.sbom {
				args = append(args, "--sbom=true")
			}
			args = append(args, plan.flags...)
			args = append(args, plan.context)

			p.log.Info("building image", slog.String("id", plan.id), slog.String("platform", platform), slog.String("image", plan.image))

			if err := p.run(ctx, env, p.cfg.Commands.Docker, args...); err != nil {
				return fmt.Errorf("build %s for %s: %w", plan.id, platform, err)
			}

			digest, err := readBuildDigest(metaFile)
			if err != nil {
				return fmt.Errorf("build %s for %s: %w", plan.id, platform, err)
			}

			results[i] = result{platform, digest}

			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return err
	}

	for _, r := range results {
		plan.digests[r.platform] = r.digest
	}

	return nil
}

func readBuildDigest(metaFile string) (string, error) {
	raw, err := os.ReadFile(metaFile)
	if err != nil {
		return "", fmt.Errorf("read buildx metadata: %w", err)
	}

	var meta struct {
		Digest string `json:"containerimage.digest"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return "", fmt.Errorf("parse buildx metadata %s: %w", metaFile, err)
	}

	if meta.Digest == "" {
		return "", fmt.Errorf("buildx metadata %s carries no containerimage.digest — was the image pushed?", metaFile)
	}

	return meta.Digest, nil
}

// tagImage writes the manifest list under the entry's tags — the one
// tag write — and reads back the digest the tags now resolve to.
func (p *Pipeline) tagImage(ctx context.Context, env []string, plan *imagePlan) (publishedImage, error) {
	args := []string{"buildx", "imagetools", "create"}
	for _, tag := range plan.tags {
		args = append(args, "--tag", plan.image+":"+tag)
	}

	for _, platform := range plan.platforms {
		args = append(args, plan.image+"@"+plan.digests[platform])
	}

	p.log.Info("tagging image", slog.String("id", plan.id), slog.String("image", plan.image), slog.String("tags", strings.Join(plan.tags, ",")))

	if err := p.run(ctx, env, p.cfg.Commands.Docker, args...); err != nil {
		return publishedImage{}, fmt.Errorf("tag %s: %w", plan.id, err)
	}

	out, err := p.output(ctx, env, p.cfg.Commands.Docker,
		"buildx", "imagetools", "inspect", plan.image+":"+plan.tags[0], "--format", "{{.Manifest.Digest}}")
	if err != nil {
		return publishedImage{}, fmt.Errorf("inspect %s: %w", plan.id, err)
	}

	digest := strings.TrimSpace(out)
	if !strings.HasPrefix(digest, "sha256:") {
		return publishedImage{}, fmt.Errorf("inspect %s: unexpected digest %q", plan.id, digest)
	}

	return publishedImage{
		ID:        plan.id,
		Image:     plan.image,
		Tags:      plan.tags,
		Digest:    digest,
		Platforms: plan.platforms,
	}, nil
}

// appendArtifacts adds the "Docker Image" entries goreleaser would have
// written — one per tag, name "repo:tag", extra.Digest the manifest-list
// digest — to dist/artifacts.json. helmctl goreleaser-manifest reads
// exactly these (ocictl pkg/goreleaserdist).
func (p *Pipeline) appendArtifacts(images []publishedImage) error {
	path := filepath.Join(p.abs(p.cfg.DistDir), "artifacts.json")

	var artifacts []map[string]any

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &artifacts); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	case os.IsNotExist(err):
		artifacts = []map[string]any{}
	default:
		return fmt.Errorf("read %s: %w", path, err)
	}

	for _, img := range images {
		for _, tag := range img.Tags {
			ref := img.Image + ":" + tag
			artifacts = append(artifacts, map[string]any{
				"name": ref,
				"path": ref,
				"type": "Docker Image",
				"extra": map[string]any{
					"Digest":    img.Digest,
					"ID":        img.ID,
					"Platforms": img.Platforms,
				},
			})
		}
	}

	out, err := json.MarshalIndent(artifacts, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}

	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}
