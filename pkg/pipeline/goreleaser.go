package pipeline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template"

	"go.yaml.in/yaml/v3"
)

// goreleaserConfig is the slice of .goreleaser.yaml the image step needs:
// where the project's files live (monorepo.dir) and the dockers_v2
// entries themselves. Everything else in the file is goreleaser's and is
// deliberately not modelled — unknown fields are ignored on purpose, so
// a new goreleaser feature never breaks the parse here.
type goreleaserConfig struct {
	Monorepo struct {
		Dir string `yaml:"dir"`
	} `yaml:"monorepo"`
	DockersV2 []dockerV2 `yaml:"dockers_v2"`
}

// dockerV2 mirrors one dockers_v2 entry. The field set is goreleaser's
// (https://goreleaser.com/customization/dockers_v2/); the semantics the
// image step reproduces are documented on publishImages.
type dockerV2 struct {
	ID         string            `yaml:"id"`
	IDs        []string          `yaml:"ids"`
	Dockerfile string            `yaml:"dockerfile"`
	Images     []string          `yaml:"images"`
	Tags       []string          `yaml:"tags"`
	Flags      []string          `yaml:"flags"`
	Platforms  []string          `yaml:"platforms"`
	BuildArgs  map[string]string `yaml:"build_args"`
	Labels     map[string]string `yaml:"labels"`
	ExtraFiles []string          `yaml:"extra_files"`
	SBOM       string            `yaml:"sbom"`
	Disable    string            `yaml:"disable"`
}

func loadGoreleaserConfig(path string) (*goreleaserConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read goreleaser config: %w", err)
	}

	var cfg goreleaserConfig
	if err := yaml.NewDecoder(bytes.NewReader(raw)).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse goreleaser config %s: %w", path, err)
	}

	for i := range cfg.DockersV2 {
		d := &cfg.DockersV2[i]
		if d.Dockerfile == "" {
			d.Dockerfile = "Dockerfile"
		}

		if len(d.Platforms) == 0 {
			d.Platforms = []string{"linux/amd64", "linux/arm64"}
		}

		if len(d.Tags) == 0 {
			d.Tags = []string{"{{ .Tag }}"}
		}
	}

	return &cfg, nil
}

// goreleaserMetadata is dist/metadata.json — goreleaser's own account of
// the version it built, which the image step MUST reuse verbatim: the
// nightly version string (1.2.3-abcdef0-nightly) is goreleaser's to
// compute, and helmctl reads the same file when it pins the charts.
type goreleaserMetadata struct {
	ProjectName string `json:"project_name"`
	Tag         string `json:"tag"`
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	Date        string `json:"date"`
}

func loadGoreleaserMetadata(distDir string) (*goreleaserMetadata, error) {
	raw, err := os.ReadFile(filepath.Join(distDir, "metadata.json"))
	if err != nil {
		return nil, fmt.Errorf("read goreleaser metadata: %w", err)
	}

	var meta goreleaserMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("parse goreleaser metadata: %w", err)
	}

	if meta.Version == "" {
		return nil, fmt.Errorf("goreleaser metadata %s carries no version", filepath.Join(distDir, "metadata.json"))
	}

	return &meta, nil
}

// templateData is the subset of goreleaser's template context the image
// entries in this estate use: the version fields, the release-kind
// booleans and the environment. Field names are goreleaser's, so a
// .goreleaser.yaml keeps rendering identically under goreleaser on a
// laptop and under this step in a flow.
type templateData struct {
	ProjectName string
	Version     string
	Tag         string
	Commit      string
	ShortCommit string
	Date        string
	IsSnapshot  bool
	IsNightly   bool
	Env         map[string]string
}

func newTemplateData(meta *goreleaserMetadata, nightly bool, env []string) templateData {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			m[k] = v
		}
	}

	short := meta.Commit
	if len(short) > 7 {
		short = short[:7]
	}

	return templateData{
		ProjectName: meta.ProjectName,
		Version:     meta.Version,
		Tag:         meta.Tag,
		Commit:      meta.Commit,
		ShortCommit: short,
		Date:        meta.Date,
		IsNightly:   nightly,
		Env:         m,
	}
}

// render evaluates one goreleaser template string. Two goreleaser funcs
// are provided because the estate's configs use them: envOrDefault (the
// warm-cache flags) and the plain .Env map lookup, which yields "" for
// an unset variable instead of "<no value>".
func (d templateData) render(s string) (string, error) {
	funcs := template.FuncMap{
		"envOrDefault": func(name, def string) string {
			if v, ok := d.Env[name]; ok && v != "" {
				return v
			}

			return def
		},
	}

	t, err := template.New("goreleaser").Funcs(funcs).Option("missingkey=zero").Parse(s)
	if err != nil {
		return "", fmt.Errorf("parse template %q: %w", s, err)
	}

	var out bytes.Buffer
	if err := t.Execute(&out, d); err != nil {
		return "", fmt.Errorf("render template %q: %w", s, err)
	}

	return out.String(), nil
}

// renderAll renders a list and drops entries that render empty — the
// convention goreleaser's own tag/flag lists follow (`{{ if ... }}latest{{ end }}`
// contributes nothing outside a stable release).
func (d templateData) renderAll(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, s := range in {
		v, err := d.render(s)
		if err != nil {
			return nil, err
		}

		if strings.TrimSpace(v) != "" {
			out = append(out, strings.TrimSpace(v))
		}
	}

	return out, nil
}

// projectDir is where the entry's relative paths (dockerfile, extra_files)
// resolve: goreleaser Pro's monorepo.dir, or the repository root.
func (c *goreleaserConfig) projectDir(root string) string {
	if c.Monorepo.Dir == "" || c.Monorepo.Dir == "." {
		return root
	}

	return filepath.Join(root, filepath.FromSlash(path.Clean(c.Monorepo.Dir)))
}
