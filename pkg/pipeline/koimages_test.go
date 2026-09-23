package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testKoGoreleaserYAML = `
project_name: url-shortener
dist: dist/url-shortener
monorepo:
  tag_prefix: url-shortener/
  dir: url-shortener
builds:
  - id: url-shortener-redirect
    main: ./cmd/url-shortener/redirect
    flags: ["-trimpath"]
    ldflags: ["-s", "-w", "-X main.Version={{.Version}}"]
    env: ["CGO_ENABLED=0"]
kos:
  - id: url-shortener-redirect
    build: url-shortener-redirect
    tags: ["{{ .Version }}", "{{ if and (not .IsSnapshot) (not .IsNightly) }}latest{{ end }}"]
    platforms: [linux/amd64, linux/arm64]
    # BOTH, which is what every config in this estate writes. ko resolves
    # it to base_import_paths; a reader taking bare first would name the
    # image after the repository's parent.
    bare: true
    preserve_import_paths: false
    base_import_paths: true
    base_image: gcr.io/distroless/static:nonroot
    sbom: none
    labels:
      org.opencontainers.image.version: "{{ .Version }}"
      org.opencontainers.image.title: redirect
  - id: url-shortener-off
    build: url-shortener-redirect
    disable: "true"
`

func seedKoProject(t *testing.T, root, version string) {
	t.Helper()

	proj := filepath.Join(root, "url-shortener")
	require.NoError(t, os.MkdirAll(proj, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(proj, ".goreleaser.yaml"), []byte(testKoGoreleaserYAML), 0o644))

	dist := filepath.Join(root, "dist", "url-shortener")
	require.NoError(t, os.MkdirAll(dist, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dist, "metadata.json"),
		[]byte(`{"project_name":"url-shortener","tag":"v1.2.3","version":"`+version+
			`","commit":"abcdef0123456789","date":"2026-09-06T00:00:00Z"}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dist, "artifacts.json"), []byte(`[]`), 0o644))
}

// stubKoPublish scripts ko: it writes the reference it "pushed" to the
// file --image-refs names, which is where the digest is read from.
func stubKoPublish(s *stubRunner, digest string) {
	s.on("ko build", stubResult{effect: func(c Command) error {
		refs := ""
		for i, a := range c.Argv {
			if a == "--image-refs" {
				refs = c.Argv[i+1]
			}
		}

		return os.WriteFile(refs, []byte("reg/url-shortener@"+digest+"\n"), 0o644)
	}})
}

// The stable release: every tag is pushed, including `latest`, and the
// digest recorded is the one ko reported rather than anything computed
// here.
func TestPublishKoImagesStable(t *testing.T) {
	p, s, root, _ := newTestPipeline(t)
	require.NoError(t, p.resolveRoot(context.Background()))
	seedKoProject(t, root, "1.2.3")
	stubKoPublish(s, "sha256:aaa")

	env := []string{"KO_DOCKER_REPO=reg/url-shortener"}

	images, err := p.publishKoImages(context.Background(), env, false)
	require.NoError(t, err)
	require.Len(t, images, 1, "the disabled entry must not be built")

	img := images[0]
	assert.Equal(t, "url-shortener-redirect", img.ID)
	assert.Equal(t, "reg/url-shortener/redirect", img.Image,
		"base_import_paths wins over bare, which is ko's own precedence")
	assert.Equal(t, []string{"1.2.3", "latest"}, img.Tags)
	assert.Equal(t, "sha256:aaa", img.Digest)

	call := s.call(t, "ko build")
	argv := strings.Join(call.Argv, " ")
	assert.Contains(t, argv, "--push")
	assert.Contains(t, argv, "--platform linux/amd64,linux/arm64")
	assert.Contains(t, argv, "--tags 1.2.3,latest")
	assert.Contains(t, argv, "--base-import-paths")
	assert.NotContains(t, argv, "--bare", "exactly one naming flag, or ko makes the choice instead")
	assert.Contains(t, argv, "--sbom none")
	assert.Contains(t, argv, "./cmd/url-shortener/redirect")

	// Labels are sorted, so two runs of one configuration produce the
	// same argv and therefore the same image.
	assert.Contains(t, argv, "--image-label org.opencontainers.image.title=redirect "+
		"--image-label org.opencontainers.image.version=1.2.3")

	// The build's flags, ldflags and env reach the compiler.
	assert.Contains(t, call.Env, "GOFLAGS=-trimpath")
	assert.Contains(t, call.Env, "GOLDFLAGS=-s -w -X main.Version=1.2.3")
	assert.Contains(t, call.Env, "CGO_ENABLED=0")
	assert.Contains(t, call.Env, "KO_DEFAULTBASEIMAGE=gcr.io/distroless/static:nonroot")
	assert.Contains(t, call.Env, "KO_DOCKER_REPO=reg/url-shortener")
}

// The dev loop. THE regression this guards: a snapshot must not move
// `latest`. The tag is suppressed by the config's own template, and that
// template only works if the context says IsSnapshot — which it did not
// while the flow used the Pro-only --nightly.
func TestPublishKoImagesSnapshotDropsLatest(t *testing.T) {
	p, s, root, _ := newTestPipeline(t)
	require.NoError(t, p.resolveRoot(context.Background()))
	seedKoProject(t, root, "1.2.4-abcdef0-nightly")
	stubKoPublish(s, "sha256:bbb")

	images, err := p.publishKoImages(context.Background(), []string{"KO_DOCKER_REPO=reg/url-shortener"}, true)
	require.NoError(t, err)
	require.Len(t, images, 1)

	assert.Equal(t, []string{"1.2.4-abcdef0-nightly"}, images[0].Tags)
	assert.NotContains(t, strings.Join(s.call(t, "ko build").Argv, " "), "latest")
}

// The artifacts.json entries helmctl reads: one "Docker Image" per tag,
// named repo:tag, with the digest ko reported. Without these the charts
// pin nothing.
func TestPublishKoImagesWritesArtifacts(t *testing.T) {
	p, s, root, _ := newTestPipeline(t)
	require.NoError(t, p.resolveRoot(context.Background()))
	seedKoProject(t, root, "1.2.3")
	stubKoPublish(s, "sha256:ccc")

	_, err := p.publishKoImages(context.Background(), []string{"KO_DOCKER_REPO=reg/url-shortener"}, false)
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join(root, "dist", "url-shortener", "artifacts.json"))
	require.NoError(t, err)

	var artifacts []struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		Extra struct {
			Digest    string   `json:"Digest"`
			ID        string   `json:"ID"`
			Platforms []string `json:"Platforms"`
		} `json:"extra"`
	}

	require.NoError(t, json.Unmarshal(raw, &artifacts))
	require.Len(t, artifacts, 2)
	assert.Equal(t, "reg/url-shortener/redirect:1.2.3", artifacts[0].Name)
	assert.Equal(t, "Docker Image", artifacts[0].Type)
	assert.Equal(t, "sha256:ccc", artifacts[0].Extra.Digest)
	assert.Equal(t, "url-shortener-redirect", artifacts[0].Extra.ID)
	assert.Equal(t, []string{"linux/amd64", "linux/arm64"}, artifacts[0].Extra.Platforms)
	assert.Equal(t, "reg/url-shortener/redirect:latest", artifacts[1].Name)
}

// A ko entry naming a build the config does not declare would otherwise
// be built from goreleaser's defaults, which compile the wrong main.
func TestPublishKoImagesRefusesAnUnknownBuild(t *testing.T) {
	p, s, root, _ := newTestPipeline(t)
	require.NoError(t, p.resolveRoot(context.Background()))
	seedKoProject(t, root, "1.2.3")
	stubKoPublish(s, "sha256:ddd")

	cfgPath := filepath.Join(root, "url-shortener", ".goreleaser.yaml")
	raw, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cfgPath,
		[]byte(strings.Replace(string(raw), "build: url-shortener-redirect", "build: nope", 1)), 0o644))

	_, err = p.publishKoImages(context.Background(), []string{"KO_DOCKER_REPO=reg/url-shortener"}, false)
	require.ErrorContains(t, err, "which this config does not declare")
}

// No repository anywhere is a refusal, not a push to a default nobody
// chose.
func TestPublishKoImagesRefusesWithoutARepository(t *testing.T) {
	p, s, root, _ := newTestPipeline(t)
	require.NoError(t, p.resolveRoot(context.Background()))
	seedKoProject(t, root, "1.2.3")
	stubKoPublish(s, "sha256:eee")

	_, err := p.publishKoImages(context.Background(), nil, false)
	require.ErrorContains(t, err, "KO_DOCKER_REPO is not set")
}

// ko reporting no reference means it pushed nothing. Taking that as
// success would pin the charts to an empty digest.
func TestPublishKoImagesRefusesWhenKoPushedNothing(t *testing.T) {
	p, s, root, _ := newTestPipeline(t)
	require.NoError(t, p.resolveRoot(context.Background()))
	seedKoProject(t, root, "1.2.3")
	s.on("ko build", stubResult{effect: func(c Command) error {
		for i, a := range c.Argv {
			if a == "--image-refs" {
				return os.WriteFile(c.Argv[i+1], nil, 0o644)
			}
		}

		return nil
	}})

	_, err := p.publishKoImages(context.Background(), []string{"KO_DOCKER_REPO=reg/url-shortener"}, false)
	require.ErrorContains(t, err, "pushed nothing")
}

// bare ALONE is the repository verbatim, with no component suffix. This
// is the other half of the precedence: with base_import_paths off, bare
// is what names the image.
func TestPublishKoImagesBareAlone(t *testing.T) {
	p, s, root, _ := newTestPipeline(t)
	require.NoError(t, p.resolveRoot(context.Background()))
	seedKoProject(t, root, "1.2.3")
	stubKoPublish(s, "sha256:fff")

	cfgPath := filepath.Join(root, "url-shortener", ".goreleaser.yaml")
	raw, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cfgPath,
		[]byte(strings.Replace(string(raw), "base_import_paths: true", "base_import_paths: false", 1)), 0o644))

	images, err := p.publishKoImages(context.Background(), []string{"KO_DOCKER_REPO=reg/url-shortener"}, false)
	require.NoError(t, err)
	assert.Equal(t, "reg/url-shortener", images[0].Image)

	argv := strings.Join(s.call(t, "ko build").Argv, " ")
	assert.Contains(t, argv, "--bare")
	assert.NotContains(t, argv, "--base-import-paths")
}

// preserve_import_paths beats both, which is the top of ko's precedence.
func TestPublishKoImagesPreserveImportPathsWinsOverBoth(t *testing.T) {
	p, s, root, _ := newTestPipeline(t)
	require.NoError(t, p.resolveRoot(context.Background()))
	seedKoProject(t, root, "1.2.3")
	stubKoPublish(s, "sha256:999")

	cfgPath := filepath.Join(root, "url-shortener", ".goreleaser.yaml")
	raw, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cfgPath,
		[]byte(strings.Replace(string(raw), "preserve_import_paths: false", "preserve_import_paths: true", 1)), 0o644))

	images, err := p.publishKoImages(context.Background(), []string{"KO_DOCKER_REPO=reg/url-shortener"}, false)
	require.NoError(t, err)
	assert.Equal(t, "reg/url-shortener/cmd/url-shortener/redirect", images[0].Image)
	assert.Contains(t, strings.Join(s.call(t, "ko build").Argv, " "), "--preserve-import-paths")
}
