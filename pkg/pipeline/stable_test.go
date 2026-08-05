package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testHeadSha = "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"
	testTagSha  = "bbbb2222bbbb2222bbbb2222bbbb2222bbbb2222"
	testTag     = "url-shortener/v1.2.3"
)

// stubGatesPass scripts a git state that satisfies all five gates.
func stubGatesPass(s *stubRunner) {
	s.on("git status --porcelain", stubResult{out: ""})
	s.on("git fetch origin master --tags", stubResult{})
	s.on("git rev-parse HEAD", stubResult{out: testHeadSha + "\n"})
	s.on("git rev-parse origin/master", stubResult{out: testHeadSha + "\n"})
	s.on("git describe --exact-match --match url-shortener/v* --tags HEAD", stubResult{out: testTag + "\n"})
	s.on("git ls-remote origin refs/tags/"+testTag, stubResult{out: testTagSha + "\trefs/tags/" + testTag + "\n"})
	s.on("git rev-parse refs/tags/"+testTag, stubResult{out: testTagSha + "\n"})
}

// stubStableBuild scripts the full happy build+push on top of passing
// gates.
func stubStableBuild(t *testing.T, s *stubRunner, root string) {
	t.Helper()

	stubGatesPass(s)
	stubBuildEffects(t, s, root, "goreleaser release --clean -f url-shortener/.goreleaser.yaml", "1.2.3")
	s.on("aws ecr get-login-password --profile stable@power --region eu-central-1", stubResult{out: "sekret\n"})
}

// TestStableGatesFailFast is the table over gates a–d: each one
// individually stops the release BEFORE any build step or credential
// use — the only commands that may have run are git.
func TestStableGatesFailFast(t *testing.T) {
	cases := []struct {
		name       string
		override   func(s *stubRunner)
		wantErr    string
		wantStderr []string
	}{
		{
			name: "gate a: dirty working tree",
			override: func(s *stubRunner) {
				s.on("git status --porcelain", stubResult{out: " M url-shortener/src/main.ts\n?? scratch.txt\n"})
			},
			wantErr: "working tree is not clean",
			wantStderr: []string{
				"working tree is not clean (uncommitted changes or untracked files present)",
				"builds EXACTLY what the release tag points at",
				"hint: for iterating on uncommitted work use the dev loop",
				"gemaalctl pipeline snapshot",
			},
		},
		{
			name: "gate b: HEAD is not origin/master",
			override: func(s *stubRunner) {
				s.on("git rev-parse origin/master", stubResult{out: "cccc3333cccc3333cccc3333cccc3333cccc3333\n"})
			},
			wantErr: "HEAD is not origin/master",
			wantStderr: []string{
				"HEAD is not origin/master (after fetch)",
				"Stable releases are cut only from the pushed tip of master",
			},
		},
		{
			name: "gate b: fetch fails (diverged tag clobber)",
			override: func(s *stubRunner) {
				s.on("git fetch origin master --tags", stubResult{err: errors.New("would clobber existing tag")})
			},
			wantErr: "clobber",
		},
		{
			name: "gate c: no release tag on HEAD",
			override: func(s *stubRunner) {
				s.on("git describe --exact-match", stubResult{err: errors.New("fatal: no tag exactly matches")})
			},
			wantErr: "carries no url-shortener/v* tag",
			wantStderr: []string{
				"HEAD carries no 'url-shortener/v*' tag",
				"git tag -a url-shortener/vX.Y.Z",
			},
		},
		{
			name: "gate d: tag never pushed",
			override: func(s *stubRunner) {
				s.on("git ls-remote origin refs/tags/"+testTag, stubResult{out: ""})
			},
			wantErr: "never pushed to origin",
			wantStderr: []string{
				"exists locally but was never pushed to origin",
				"An unpushed tag is not a release",
				"hint: git push origin 'url-shortener/v1.2.3'",
			},
		},
		{
			name: "gate d: tag diverged between local and origin",
			override: func(s *stubRunner) {
				s.on("git rev-parse refs/tags/"+testTag, stubResult{out: "dddd4444dddd4444dddd4444dddd4444dddd4444\n"})
			},
			wantErr: "differs between local and origin",
			wantStderr: []string{
				"differs between local and origin",
				"a moved release tag is an incident, not an inconvenience",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, s, _, stderr := newTestPipeline(t)
			stubGatesPass(s)
			tc.override(s)

			err := p.ReleaseStable(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)

			for _, want := range tc.wantStderr {
				assert.Contains(t, stderr.String(), want)
			}

			// Fail-fast means fail BEFORE: no pre-build, no goreleaser, no
			// packaging, no credential use.
			assertOnlyGitRan(t, s)

			// And no partial-publish banner — nothing started.
			assert.NotContains(t, stderr.String(), "PARTIAL RELEASE")
		})
	}
}

func TestStableHappyPath(t *testing.T) {
	p, s, root, stderr := newTestPipeline(t)
	stubStableBuild(t, s, root)

	require.NoError(t, p.ReleaseStable(context.Background()))

	// Gate e always speaks: laptop provenance is interim.
	assert.Contains(t, stderr.String(), "WARNING: stable release from a laptop")
	assert.NotContains(t, stderr.String(), "PARTIAL RELEASE")

	// Pre-build runs AFTER the gates (gates that run after a build are
	// not gates) and BEFORE goreleaser.
	preBuild := s.find("moon run url-shortener:build/app/web url-shortener:build/app/log")
	gorel := s.find("goreleaser release --clean")
	lastGit := -1

	for i, c := range s.joinedCalls() {
		if strings.HasPrefix(c, "git ") && i > lastGit {
			lastGit = i
		}
	}

	require.GreaterOrEqual(t, preBuild, 0, "calls: %v", s.joinedCalls())
	require.GreaterOrEqual(t, gorel, 0)
	assert.Greater(t, preBuild, lastGit)
	assert.Less(t, preBuild, gorel)

	// Full validation — never nightly.
	assert.False(t, s.called("goreleaser release --nightly"))

	// Stable-wired environment.
	env := s.call(t, "goreleaser release --clean").Env
	assert.Contains(t, env, "REGISTRY=stable.example.com")
	assert.Contains(t, env, "AWS_PROFILE=stable@power")
	assert.Contains(t, env, "KO_DOCKER_REPO=stable.example.com/url-shortener")

	// Publish order: login, then ring2, then ring3 (the commit point).
	login := s.find("helm registry login --username AWS --password-stdin stable.example.com")
	require.GreaterOrEqual(t, login, 0)

	var pushes []string

	for _, c := range s.joinedCalls() {
		if strings.HasPrefix(c, "go tool helmctl push") {
			pushes = append(pushes, c)
		}
	}

	require.Len(t, pushes, 2)
	assert.Contains(t, pushes[0], "--repository url-shortener/charts/url-shortener-infra")
	assert.Contains(t, pushes[1], "--repository url-shortener/charts/url-shortener ")
	assert.Less(t, login, s.find("go tool helmctl push"))

	// The build stamped `stable`.
	stamp, err := os.ReadFile(filepath.Join(root, "dist", "url-shortener", "charts", stampFileName))
	require.NoError(t, err)
	assert.Equal(t, "stable\n", string(stamp))
}

// TestStablePartialPublishBranches walks the partial-publish state
// machine through the flow by failing each stage, asserting the report
// tells the truth about what landed, what MIGHT have landed, and the
// recovery.
func TestStablePartialPublishBranches(t *testing.T) {
	cases := []struct {
		name        string
		override    func(s *stubRunner)
		wantErr     string
		wantBanner  bool
		wantStderr  []string
		notInStderr []string
		stageKept   bool
	}{
		{
			// Failed before goreleaser: nothing is out there, nothing to say.
			name: "pre-build failure — no banner",
			override: func(s *stubRunner) {
				s.on("moon run", stubResult{err: errors.New("vite exploded")})
			},
			wantErr:    "vite exploded",
			wantBanner: false,
		},
		{
			// goreleaser started but never confirmed: an UNKNOWN subset of
			// images may be in the registry; the version is burned.
			name: "goreleaser dies mid-publish — new PATCH tag",
			override: func(s *stubRunner) {
				s.on("goreleaser release --clean", stubResult{err: errors.New("manifest push failed")})
			},
			wantErr:    "manifest push failed",
			wantBanner: true,
			wantStderr: []string{
				"POSSIBLY PARTIALLY PUBLISHED to stable.example.com",
				"An UNKNOWN SUBSET of the per-arch",
				"NOT published:",
				"Treat url-shortener/v1.2.3 as NOT released",
				"RECOVERY — cut a new PATCH tag",
				"the version\n  is burned",
			},
		},
		{
			// Images confirmed, packaging died: no tarball the report can
			// honestly name — advise manual packaging from the manifest.
			name: "chart packaging fails after images",
			override: func(s *stubRunner) {
				s.on("go tool helmctl package --chart url-shortener/charts/url-shortener --manifest",
					stubResult{err: errors.New("digest missing")})
			},
			wantErr:    "digest missing",
			wantBanner: true,
			wantStderr: []string{
				"ALREADY PUBLISHED to stable.example.com (cannot be rolled back here)",
				"container images for 1.2.3 (goreleaser publish finished)",
				"RECOVERY — complete chart packaging manually from the generated manifest",
				"go tool helmctl goreleaser-manifest --goreleaser-dist dist/url-shortener",
				"helm dependency update url-shortener/charts/url-shortener-infra",
				"--require-image-digests",
				"Do NOT cut\n  a new tag",
			},
		},
		{
			// Ring2 push interrupted: outcome UNKNOWN, not absent; the
			// staged tarballs are the recovery artifacts.
			name: "ring2 push fails — push by hand, stage kept",
			override: func(s *stubRunner) {
				s.on("go tool helmctl push", stubResult{err: errors.New("connection reset")})
			},
			wantErr:    "connection reset",
			wantBanner: true,
			stageKept:  true,
			wantStderr: []string{
				"POSSIBLY PUBLISHED to stable.example.com (push interrupted — outcome UNKNOWN",
				"* ring2 chart url-shortener-infra:1.2.3",
				"NOT published:",
				"* ring3 chart url-shortener  <- the commit point",
				"RECOVERY — push the missing chart(s) by hand, nothing else",
				"--repository url-shortener/charts/url-shortener-infra",
				"--repository url-shortener/charts/url-shortener ",
				"Ring2 before ring3 (install order)",
			},
		},
		{
			// Ring3 push interrupted: if it landed the release is complete;
			// the registry, not the report, holds the answer. Ring2 is
			// confirmed — its push command must NOT be re-advised.
			name: "ring3 push fails — check registry, stage kept",
			override: func(s *stubRunner) {
				// Ring2 succeeds, ring3 fails: key the failure on the
				// ring3 repository argument.
				s.on("go tool helmctl push", stubResult{effect: func(c Command) error {
					if strings.Contains(strings.Join(c.Argv, " "), "--repository url-shortener/charts/url-shortener ") {
						return errors.New("ring3 boom")
					}

					return nil
				}})
			},
			wantErr:    "ring3 boom",
			wantBanner: true,
			stageKept:  true,
			wantStderr: []string{
				"* ring2 chart url-shortener-infra:1.2.3",
				"POSSIBLY PUBLISHED to stable.example.com (push interrupted — outcome UNKNOWN",
				"* ring3 chart url-shortener:1.2.3",
				"Ring3 is the commit point and its push was interrupted",
				"RECOVERY — push the missing chart(s) by hand, nothing else",
			},
			// Ring2 CONFIRMED: no NOT-published block, and no ring2
			// re-push advice (it would die on the immutable version tag).
			notInStderr: []string{"NOT published:", "--name url-shortener-infra"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, s, root, stderr := newTestPipeline(t)
			stubStableBuild(t, s, root)
			tc.override(s)

			err := p.ReleaseStable(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)

			if tc.wantBanner {
				assert.Contains(t, stderr.String(), "PARTIAL RELEASE — url-shortener/v1.2.3 did NOT complete")
				assert.Contains(t, stderr.String(), "IMMUTABLE_WITH_EXCLUSION")
			} else {
				assert.NotContains(t, stderr.String(), "PARTIAL RELEASE")
			}

			for _, want := range tc.wantStderr {
				assert.Contains(t, stderr.String(), want)
			}

			for _, unwanted := range tc.notInStderr {
				assert.NotContains(t, stderr.String(), unwanted)
			}

			// Stage retention: kept ONLY while the report names its
			// tarballs as the manual-push input.
			stageDirs, globErr := filepath.Glob(filepath.Join(os.TempDir(), "gemaal-charts-stage-*"))
			require.NoError(t, globErr)

			if tc.stageKept {
				require.NotEmpty(t, stageDirs, "recovery stage should have been kept")
				assert.Contains(t, stderr.String(), "run's validated tarballs are staged in")

				for _, d := range stageDirs {
					require.NoError(t, os.RemoveAll(d))
				}
			} else {
				assert.Empty(t, stageDirs, "no recovery to stage — the snapshot must be removed")
			}
		})
	}
}

// TestStableRefusesForeignChartVersion ports the belt-and-braces
// cross-check: the packaged charts must carry the version THIS run's
// manifest produced.
func TestStableRefusesForeignChartVersion(t *testing.T) {
	p, s, root, stderr := newTestPipeline(t)
	stubStableBuild(t, s, root)

	// Packaging drops charts with a version this run did not build.
	chartsOut := filepath.Join(root, "dist", "url-shortener", "charts")
	s.on("go tool helmctl package --chart url-shortener/charts/url-shortener-infra", stubResult{effect: func(Command) error {
		writeChartTgz(t, filepath.Join(chartsOut, "url-shortener-infra-9.9.9.tgz"), "url-shortener-infra", "9.9.9")

		return nil
	}})
	s.on("go tool helmctl package --chart url-shortener/charts/url-shortener --manifest", stubResult{effect: func(Command) error {
		writeChartTgz(t, filepath.Join(chartsOut, "url-shortener-9.9.9.tgz"), "url-shortener", "9.9.9")

		return nil
	}})

	err := p.ReleaseStable(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "do not carry this release's version")
	assert.Contains(t, stderr.String(), "packaged charts are version '9.9.9', but this release built '1.2.3'")
	assert.Contains(t, stderr.String(), "holding charts this run did not produce")

	// Never reached a credential.
	assert.False(t, s.called("aws"))
	assert.False(t, s.called("go tool helmctl push"))
}

// TestReportRing3ConfirmedButBookkeepingDied covers the state the flow
// cannot reach through stub failures: ring3 CONFIRMED, releaseComplete
// never flipped. The release IS complete — the report must say "nothing
// to push", never advise a re-push into the immutable tag.
func TestReportRing3ConfirmedButBookkeepingDied(t *testing.T) {
	p, _, _, stderr := newTestPipeline(t)

	stage := filepath.Join(t.TempDir(), "stage")
	require.NoError(t, os.MkdirAll(stage, 0o755))

	st := &stableState{
		tag:               testTag,
		version:           "1.2.3",
		chartVersion:      "1.2.3",
		goreleaserStarted: true,
		publishedImages:   true,
		chartsSelected:    true,
		startedRing2:      true,
		confirmedRing2:    true,
		startedRing3:      true,
		confirmedRing3:    true,
		stageDir:          stage,
	}

	p.reportPartialPublish(st, errors.New("killed after final push"))

	out := stderr.String()
	assert.Contains(t, out, "ring3 chart url-shortener:1.2.3  <- the commit point")
	assert.Contains(t, out, "its push CONFIRMED before this run died")
	assert.Contains(t, out, "RECOVERY — nothing to push")
	assert.NotContains(t, out, "push --tgz")

	// Ring3 confirmed == nothing left to hand-push: the stage is removed.
	assert.NoDirExists(t, stage)
}
