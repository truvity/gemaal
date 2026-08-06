package gemaalcfg

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fullYAML = `
personalNamespace: dev-{slug}
clusters:
  devel@oidc:
    values:
      region: eu-central-1
      nested:
        flag: true
hooks:
  build: [moon, run, ':build']
projects:
  url-shortener:
    hooks:
      build: [moon, run, 'url-shortener:artifacts/snapshot']
identity:
  emails:
    j.doe@example.com: jdoe
server: http://gemaal.gemaal-system.svc:8080
`

func TestParseFull(t *testing.T) {
	cfg, err := Parse([]byte(fullYAML))
	require.NoError(t, err)

	assert.Equal(t, "dev-{slug}", cfg.PersonalNamespace)
	assert.Equal(t, "eu-central-1", cfg.Clusters["devel@oidc"].Values["region"])
	assert.Equal(t, map[string]any{"flag": true}, cfg.Clusters["devel@oidc"].Values["nested"])
	assert.Equal(t, []string{"moon", "run", ":build"}, cfg.Hooks.Build)
	assert.Equal(t, map[string]string{"j.doe@example.com": "jdoe"}, cfg.Identity.Emails)
	assert.Equal(t, "http://gemaal.gemaal-system.svc:8080", cfg.Server)
}

func TestParseEmpty(t *testing.T) {
	for _, doc := range []string{"", "{}"} {
		cfg, err := Parse([]byte(doc))
		require.NoError(t, err, "doc %q", doc)
		assert.Empty(t, cfg.PersonalNamespace)
	}
}

func TestParseValidation(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "template without slug placeholder",
			yaml:    "personalNamespace: emp-static",
			wantErr: "must contain {slug}",
		},
		{
			name: "identity with two sources",
			yaml: "identity:\n  emails: {j.doe@example.com: jdoe}\n  file: identity.yaml",
			// One map, one source: both set is ambiguous.
			wantErr: "exactly one source",
		},
		{
			name:    "identity key not an email",
			yaml:    "identity:\n  emails: {jdoe: jdoe}",
			wantErr: "not an email",
		},
		{
			name:    "slug not a label",
			yaml:    "identity:\n  emails: {j.doe@example.com: J_doe}",
			wantErr: "not a valid RFC1123 label",
		},
		{
			name:    "unknown field is a typo",
			yaml:    "personalNamespac: emp-{slug}",
			wantErr: "personalNamespac",
		},
		{
			name:    "server without a scheme",
			yaml:    "server: gemaal.gemaal-system.svc:8080",
			wantErr: "http:// or https:// scheme",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestLoadMissingFileIsOptional(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "gemaal.yaml"))
	require.NoError(t, err)
	assert.NotNil(t, cfg)
	assert.Empty(t, cfg.PersonalNamespace)
}

func TestLoadUnreadableFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "gemaal.yaml"), 0o755))

	_, err := Load(filepath.Join(dir, "gemaal.yaml"))
	require.Error(t, err)
}

func TestBuildHook(t *testing.T) {
	cfg, err := Parse([]byte(fullYAML))
	require.NoError(t, err)

	tests := []struct {
		name    string
		project string
		want    []string
	}{
		{name: "project override", project: "url-shortener", want: []string{"moon", "run", "url-shortener:artifacts/snapshot"}},
		{name: "unknown project falls back to global", project: "other", want: []string{"moon", "run", ":build"}},
		{name: "empty project asks for global", project: "", want: []string{"moon", "run", ":build"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, cfg.BuildHook(tt.project))
		})
	}
}

func TestClusterValues(t *testing.T) {
	cfg, err := Parse([]byte(fullYAML))
	require.NoError(t, err)

	assert.Equal(t, "eu-central-1", cfg.ClusterValues("devel@oidc")["region"])
	assert.Nil(t, cfg.ClusterValues("nowhere"))
}

func TestEmailSlugsInline(t *testing.T) {
	cfg, err := Parse([]byte(fullYAML))
	require.NoError(t, err)

	m, err := cfg.EmailSlugs()
	require.NoError(t, err)
	assert.Equal(t, "jdoe", m["j.doe@example.com"])
}

func TestEmailSlugsNone(t *testing.T) {
	cfg, err := Parse([]byte("{}"))
	require.NoError(t, err)

	m, err := cfg.EmailSlugs()
	require.NoError(t, err)
	assert.Nil(t, m)
}

func TestEmailSlugsFromFile(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "identity.yaml"),
		[]byte("j.doe@example.com: jdoe\na.roe@example.com: aroe\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gemaal.yaml"),
		[]byte("identity:\n  file: identity.yaml\n"), 0o600))

	cfg, err := Load(filepath.Join(dir, "gemaal.yaml"))
	require.NoError(t, err)

	// The relative pointer resolves against the CONFIG's directory, not
	// the process working directory.
	m, err := cfg.EmailSlugs()
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"j.doe@example.com": "jdoe", "a.roe@example.com": "aroe"}, m)
}

func TestEmailSlugsFileMissing(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gemaal.yaml"),
		[]byte("identity:\n  file: nowhere.yaml\n"), 0o600))

	cfg, err := Load(filepath.Join(dir, "gemaal.yaml"))
	require.NoError(t, err)

	// gemaal.yaml is optional; a file it POINTS AT is promised.
	_, err = cfg.EmailSlugs()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "identity.file")
}

func TestEmailSlugsFileValidated(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "identity.yaml"),
		[]byte("j.doe@example.com: NOT_A_SLUG\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gemaal.yaml"),
		[]byte("identity:\n  file: identity.yaml\n"), 0o600))

	cfg, err := Load(filepath.Join(dir, "gemaal.yaml"))
	require.NoError(t, err)

	_, err = cfg.EmailSlugs()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RFC1123")
}
