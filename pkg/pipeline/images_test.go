package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testGoreleaserYAML = `
project_name: url-shortener
dist: dist/url-shortener
monorepo:
  tag_prefix: url-shortener/
  dir: url-shortener
kos:
  - id: url-shortener-redirect
dockers_v2:
  - id: url-shortener-web
    ids: []
    dockerfile: Dockerfile
    images:
      - "{{ .Env.REGISTRY }}/url-shortener/web"
    tags:
      - "{{ .Version }}"
      - "{{ if and (not .IsSnapshot) (not .IsNightly) }}latest{{ end }}"
    flags:
      - '{{ with envOrDefault "CI_BUILDX_CACHE" "" }}--cache-from=type=registry,ref={{ . }}:url-shortener-web{{ end }}'
    platforms:
      - linux/amd64
      - linux/arm64
    build_args:
      COMPONENT: web
    labels:
      org.opencontainers.image.version: "{{ .Version }}"
    sbom: none
    extra_files:
      - bundle/web
  - id: url-shortener-off
    disable: "true"
    images: ["{{ .Env.REGISTRY }}/url-shortener/off"]
`

// seedImageProject lays down what goreleaser leaves behind after
// `release --skip=docker`: metadata.json, an artifacts.json with the ko
// entry, plus the project files the dockers_v2 entry stages.
func seedImageProject(t *testing.T, root, version string) {
	t.Helper()

	proj := filepath.Join(root, "url-shortener")
	require.NoError(t, os.MkdirAll(filepath.Join(proj, "bundle", "web", "assets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(proj, ".goreleaser.yaml"), []byte(testGoreleaserYAML), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(proj, "Dockerfile"), []byte("FROM scratch\nCOPY bundle/web/ /app/\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(proj, "bundle", "web", "index.js"), []byte("js"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(proj, "bundle", "web", "assets", "a.css"), []byte("css"), 0o644))
	// A workspace-style node_modules: a link to a directory and a link
	// back up the tree (a cycle if followed) — dms#20's failure shape.
	require.NoError(t, os.MkdirAll(filepath.Join(proj, "bundle", "web", "node_modules"), 0o755))
	require.NoError(t, os.Symlink("../assets", filepath.Join(proj, "bundle", "web", "node_modules", "assets-link")))
	require.NoError(t, os.Symlink("../..", filepath.Join(proj, "bundle", "web", "node_modules", "up")))

	dist := filepath.Join(root, "dist", "url-shortener")
	require.NoError(t, os.MkdirAll(dist, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dist, "metadata.json"),
		[]byte(`{"project_name":"url-shortener","tag":"v1.2.3","version":"`+version+`","commit":"abcdef0123456789","date":"2026-09-06T00:00:00Z"}`), 0o644))
	koEntry := `[{"name":"reg/url-shortener/redirect@sha256:ko","type":"Docker Manifest",` +
		`"path":"reg/url-shortener/redirect@sha256:ko","extra":{"Digest":"sha256:ko","ID":"url-shortener-redirect"}}]`
	require.NoError(t, os.WriteFile(filepath.Join(dist, "artifacts.json"), []byte(koEntry), 0o644))
}

// stubImagePublish scripts buildx: each per-platform build writes the
// metadata file its --metadata-file names; imagetools inspect answers
// the manifest-list digest.
func stubImagePublish(s *stubRunner) {
	s.on("docker buildx build", stubResult{effect: func(c Command) error {
		var meta string
		platform := ""
		for i, a := range c.Argv {
			switch a {
			case "--metadata-file":
				meta = c.Argv[i+1]
			case "--platform":
				platform = c.Argv[i+1]
			}
		}

		return os.WriteFile(meta, []byte(`{"containerimage.digest":"sha256:`+strings.ReplaceAll(platform, "/", "-")+`"}`), 0o644)
	}})
	s.on("docker buildx imagetools create", stubResult{})
	// `imagetools inspect --raw` returns the exact manifest bytes the
	// registry serves; tagImage takes their sha256 as the digest. The
	// stub serves a fixed index blob, and tests assert the digest equals
	// its hash (rawIndexBytes / rawIndexDigest), so the assertion owes
	// nothing to a hand-copied sha256.
	s.on("docker buildx imagetools inspect", stubResult{out: rawIndexBytes})
}

// rawIndexBytes is a stand-in for what `imagetools inspect --raw` emits:
// the verbatim bytes of an OCI image index (a build with attestations),
// with no trailing newline — buildx suppresses it for this use. Built
// from a per-manifest helper so no single source line runs long.
var rawIndexBytes = `{"schemaVersion":2,` +
	`"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` +
	rawIndexManifest("sha256:3f8f61749c4a930dca6a5e6f38055945697f36abc051347423db27bdff4a2bd2", "amd64", "linux") + "," +
	rawIndexManifest("sha256:2e84142cecd8a251335bb03b556dd8430dc3893eca7d9b5a63d57ec146ff90f5", "unknown", "unknown") + "," +
	rawIndexManifest("sha256:92172740fde376e32cf5dd514ecb70d400b2b4369d75b26ca1b1bfdabd22b99b", "arm64", "linux") +
	`]}`

// rawIndexManifest renders one descriptor of the index above.
func rawIndexManifest(digest, arch, os string) string {
	return `{"mediaType":"application/vnd.oci.image.manifest.v1+json",` +
		`"digest":"` + digest + `",` +
		`"platform":{"architecture":"` + arch + `","os":"` + os + `"}}`
}

// rawIndexDigest is the digest the registry serves that index under:
// sha256 of exactly rawIndexBytes, the same value tagImage computes.
func rawIndexDigest() string {
	sum := sha256.Sum256([]byte(rawIndexBytes))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestPublishImagesDigestFirstTagOnce(t *testing.T) {
	p, s, root, _ := newTestPipeline(t)
	require.NoError(t, p.resolveRoot(context.Background()))
	seedImageProject(t, root, "1.2.3")
	stubImagePublish(s)
	t.Setenv("CI_BUILDX_CACHE", "reg/ci/buildkit-cache")

	env := []string{"REGISTRY=reg"}
	images, err := p.publishImages(context.Background(), env, false)
	require.NoError(t, err)

	// The disabled entry never renders; the live one is published once.
	require.Len(t, images, 1)
	assert.Equal(t, "reg/url-shortener/web", images[0].Image)
	assert.Equal(t, []string{"1.2.3", "latest"}, images[0].Tags)
	assert.Equal(t, rawIndexDigest(), images[0].Digest)

	// One build per platform, each pushed BY DIGEST — never a tag.
	calls := s.joinedCalls()
	var builds []string
	for _, c := range calls {
		if strings.HasPrefix(c, "docker buildx build") {
			builds = append(builds, c)
		}
	}
	require.Len(t, builds, 2)
	for _, b := range builds {
		assert.Contains(t, b, "--output type=image,name=reg/url-shortener/web,push=true,push-by-digest=true,oci-mediatypes=true")
		assert.Contains(t, b, "--build-arg COMPONENT=web")
		assert.Contains(t, b, "--label org.opencontainers.image.version=1.2.3")
		assert.Contains(t, b, "--cache-from=type=registry,ref=reg/ci/buildkit-cache:url-shortener-web")
		assert.NotContains(t, b, "--sbom")
		assert.NotContains(t, b, "--tag")
		assert.NotContains(t, b, ":1.2.3")
	}

	// The ONE tag write, after both digests, naming both.
	tag := s.call(t, "docker buildx imagetools create")
	joined := strings.Join(tag.Argv, " ")
	assert.Contains(t, joined, "--tag reg/url-shortener/web:1.2.3")
	assert.Contains(t, joined, "--tag reg/url-shortener/web:latest")
	assert.Contains(t, joined, "reg/url-shortener/web@sha256:linux-amd64")
	assert.Contains(t, joined, "reg/url-shortener/web@sha256:linux-arm64")
	assert.Greater(t, s.find("docker buildx imagetools create"), s.find("docker buildx build"))

	// The context: Dockerfile at the root, extra_files at their paths.
	ctxDir := filepath.Join(root, "dist", "url-shortener", "docker", "url-shortener-web")
	assert.FileExists(t, filepath.Join(ctxDir, "Dockerfile"))
	assert.FileExists(t, filepath.Join(ctxDir, "bundle", "web", "index.js"))
	assert.FileExists(t, filepath.Join(ctxDir, "bundle", "web", "assets", "a.css"))
	for _, l := range []string{"assets-link", "up"} {
		fi, err := os.Lstat(filepath.Join(ctxDir, "bundle", "web", "node_modules", l))
		require.NoError(t, err)
		assert.NotZero(t, fi.Mode()&os.ModeSymlink, "%s copied as a symlink, not followed", l)
	}

	// artifacts.json: the ko entry kept, one "Docker Image" per tag added
	// in goreleaser's own shape (helmctl reads name + extra.Digest).
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
	require.Len(t, artifacts, 3)
	assert.Equal(t, "Docker Manifest", artifacts[0].Type)
	assert.Equal(t, "reg/url-shortener/web:1.2.3", artifacts[1].Name)
	assert.Equal(t, "Docker Image", artifacts[1].Type)
	assert.Equal(t, rawIndexDigest(), artifacts[1].Extra.Digest)
	assert.Equal(t, "url-shortener-web", artifacts[1].Extra.ID)
	assert.Equal(t, []string{"linux/amd64", "linux/arm64"}, artifacts[1].Extra.Platforms)
	assert.Equal(t, "reg/url-shortener/web:latest", artifacts[2].Name)
}

func TestPublishImagesNightlyDropsLatestAndCacheFlags(t *testing.T) {
	p, s, root, _ := newTestPipeline(t)
	require.NoError(t, p.resolveRoot(context.Background()))
	seedImageProject(t, root, "1.2.4-abcdef0-nightly")
	stubImagePublish(s)
	t.Setenv("CI_BUILDX_CACHE", "")

	images, err := p.publishImages(context.Background(), []string{"REGISTRY=reg"}, true)
	require.NoError(t, err)
	require.Len(t, images, 1)
	assert.Equal(t, []string{"1.2.4-abcdef0-nightly"}, images[0].Tags)

	for _, c := range s.joinedCalls() {
		if strings.HasPrefix(c, "docker buildx build") {
			assert.NotContains(t, c, "--cache-from")
		}
	}
}

func TestPublishImagesTagsNothingWhenABuildFails(t *testing.T) {
	p, s, root, _ := newTestPipeline(t)
	require.NoError(t, p.resolveRoot(context.Background()))
	seedImageProject(t, root, "1.2.3")
	stubImagePublish(s)
	s.on("docker buildx build --platform linux/arm64", stubResult{err: os.ErrPermission})

	_, err := p.publishImages(context.Background(), []string{"REGISTRY=reg"}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "build url-shortener-web for linux/arm64")
	// No tag write reached the registry: nothing to burn.
	assert.False(t, s.called("docker buildx imagetools create"))
}

func TestPublishImagesRefusesBinaryContexts(t *testing.T) {
	p, s, root, _ := newTestPipeline(t)
	require.NoError(t, p.resolveRoot(context.Background()))
	seedImageProject(t, root, "1.2.3")
	stubImagePublish(s)

	cfg := strings.Replace(testGoreleaserYAML, "    ids: []\n", "    ids: [url-shortener-redirect]\n", 1)
	require.NoError(t, os.WriteFile(filepath.Join(root, "url-shortener", ".goreleaser.yaml"), []byte(cfg), 0o644))

	_, err := p.publishImages(context.Background(), []string{"REGISTRY=reg"}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "`ids`")
	assert.False(t, s.called("docker buildx build"))
}

func TestPublishImagesNoDockersV2IsANoop(t *testing.T) {
	p, s, root, _ := newTestPipeline(t)
	require.NoError(t, p.resolveRoot(context.Background()))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "url-shortener"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "url-shortener", ".goreleaser.yaml"), []byte("project_name: url-shortener\nkos:\n  - id: x\n"), 0o644))

	images, err := p.publishImages(context.Background(), nil, false)
	require.NoError(t, err)
	assert.Empty(t, images)
	assert.False(t, s.called("docker"))
}

// A build that carries SBOM/provenance attestations resolves its tag to
// an OCI image INDEX. tagImage takes the digest from the sha256 of the
// raw manifest bytes, so it is the digest the registry serves the index
// under -- the same one ArgoCD and Kargo resolve -- regardless of the
// nested per-arch and attestation manifests. This is the shape that broke
// the older `{{.Manifest.Digest}}` template, which rendered nothing for
// an index and let buildx fall back to a human-readable dump.
func TestPublishImagesReadsDigestFromAttestationIndex(t *testing.T) {
	p, s, root, _ := newTestPipeline(t)
	require.NoError(t, p.resolveRoot(context.Background()))
	seedImageProject(t, root, "1.2.3")
	stubImagePublish(s) // serves rawIndexBytes from `imagetools inspect --raw`

	images, err := p.publishImages(context.Background(), []string{"REGISTRY=reg"}, false)
	require.NoError(t, err)
	require.Len(t, images, 1)
	// The digest is the hash of the exact index bytes, not a nested one.
	assert.Equal(t, rawIndexDigest(), images[0].Digest)

	// The digest comes from --raw bytes, never a --format template: that
	// independence from buildx's output wording is the point of the fix.
	var inspect string
	for _, c := range s.joinedCalls() {
		if strings.HasPrefix(c, "docker buildx imagetools inspect") {
			inspect = c
			break
		}
	}
	require.NotEmpty(t, inspect, "imagetools inspect was never called")
	assert.Contains(t, inspect, "--raw")
	assert.NotContains(t, inspect, "--format")
}
