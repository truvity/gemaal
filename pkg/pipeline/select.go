package pipeline

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// selection is a proven-coherent chart set: exactly one tarball per
// CONFIGURED chart, all of them on the one version a single build
// produced.
type selection struct {
	InfraTgz string // ring2
	AppTgz   string // ring3
	// Extra holds the non-ring charts in config order. They carry no
	// ordering of their own; the flows push them between the rings only
	// so ring3, the commit point, stays last.
	Extra   []chartTarball
	Version string
}

// chartTarball pairs a configured chart name with the tarball proven to
// carry it.
type chartTarball struct {
	Name string
	Tgz  string
}

// chartYamlMember matches helm's packing layout: <chart>/Chart.yaml at
// the archive root (the scripts' `grep -x '[^/]*/Chart\.yaml'`).
var chartYamlMember = regexp.MustCompile(`^[^/]*/Chart\.yaml$`)

// maxChartYamlSize bounds how much of a tarball member is read while
// verifying identity — Chart.yaml is a few hundred bytes.
const maxChartYamlSize = 1 << 20

// chartMatch is one tarball the selector matched to a configured chart.
type chartMatch struct {
	tgz     string
	version string
}

// selectRingCharts is the port of charts-select.sh: pick this build's
// tarball for every CONFIGURED chart out of a packaged charts directory
// and prove the set is COHERENT. Used by BOTH push paths so the two
// cannot drift apart.
//
// The charts output is a REUSED build directory, not a fresh temp dir,
// so "whatever .tgz files happen to be there" is not a safe input:
//
//   - two tarballs for the same chart — an older version left behind by a
//     partially cleaned build — would silently publish whichever one a
//     glob happened to match last;
//   - tarballs from DIFFERENT builds would publish an incoherent set
//     claiming two different release versions;
//   - a file merely NAMED like a chart tarball would be published under a
//     name and version the chart inside it does not carry.
//
// chartsDir is the directory actually read — for both push paths a
// private staged snapshot. displayDir is the directory NAMED in error
// messages: naming the transient stage would point the operator at a
// path deleted the moment the flow exits, while the rebuild that fixes
// the problem happens in the REAL output dir.
//
// Matching is by longest "<name>-" prefix (Charts.match): chart names of
// one repo prefix each other, and the pair's hand-written ring2-first
// ordering does not generalize to an open-ended extra list.
func (p *Pipeline) selectRingCharts(chartsDir, rebuildHint, displayDir string) (*selection, error) {
	charts := p.cfg.Charts

	entries, err := os.ReadDir(chartsDir)
	if err != nil {
		return nil, fmt.Errorf("read charts dir: %w", err)
	}

	matched := make(map[string]chartMatch, len(charts.all()))

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".tgz") {
			continue
		}

		// Regular files only: a directory named *.tgz is not a chart. A
		// skipped impostor then fails the completeness check below with
		// the rebuild hint.
		if !entry.Type().IsRegular() {
			continue
		}

		base := strings.TrimSuffix(name, ".tgz")

		owner, version, ok := charts.match(base)
		if !ok {
			p.errf("error: unexpected chart tgz '%s' in %s\n", base, displayDir)
			p.errf("hint: rebuild with '%s'\n", rebuildHint)

			return nil, fmt.Errorf("unexpected chart tgz %s", base)
		}

		if previous, duplicate := matched[owner]; duplicate {
			label := charts.label(owner)
			p.duplicateChartError(label, previous.tgz, base, displayDir, rebuildHint)

			return nil, fmt.Errorf("more than one %s chart", label)
		}

		matched[owner] = chartMatch{tgz: filepath.Join(chartsDir, name), version: version}
	}

	// Every configured chart must be present — a partially packaged
	// directory must never publish half a build.
	var missing []string

	for _, ch := range charts.all() {
		if _, ok := matched[ch.Name]; !ok {
			missing = append(missing, fmt.Sprintf("%s-*.tgz (%s)", ch.Name, charts.label(ch.Name)))
		}
	}

	if len(missing) > 0 {
		p.errf("error: incomplete chart set in %s — missing: %s\n", displayDir, strings.Join(missing, " "))
		p.errf("hint: rebuild with '%s'\n", rebuildHint)

		return nil, errors.New("incomplete chart set")
	}

	if err := p.checkVersions(matched, displayDir, rebuildHint); err != nil {
		return nil, err
	}

	// Every check above trusted FILENAMES, and a file can lie. Prove each
	// selected tarball actually contains the chart its name claims before
	// anything downstream treats it as one.
	for _, ch := range charts.all() {
		m := matched[ch.Name]
		if err := p.verifyChartIdentity(m.tgz, ch.Name, m.version, displayDir, rebuildHint); err != nil {
			return nil, err
		}
	}

	sel := &selection{
		InfraTgz: matched[charts.Ring2.Name].tgz,
		AppTgz:   matched[charts.Ring3.Name].tgz,
		Version:  matched[charts.Ring2.Name].version,
	}

	for _, ch := range charts.Extra {
		sel.Extra = append(sel.Extra, chartTarball{Name: ch.Name, Tgz: matched[ch.Name].tgz})
	}

	return sel, nil
}

// checkVersions proves the whole set came out of ONE build: every
// tarball carries a version, and they all carry the SAME one.
func (p *Pipeline) checkVersions(matched map[string]chartMatch, displayDir, rebuildHint string) error {
	charts := p.cfg.Charts

	for _, ch := range charts.all() {
		if matched[ch.Name].version != "" {
			continue
		}

		p.errf("error: unversioned chart tarball in %s\n", displayDir)

		for _, c := range charts.all() {
			p.errf("       %s: %s\n", charts.label(c.Name), filepath.Base(matched[c.Name].tgz))
		}

		p.errf("hint: rebuild with '%s'\n", rebuildHint)

		return errors.New("unversioned chart tarball")
	}

	// The rings first, and by themselves: they are the pair whose
	// incoherence is a mismatched RELEASE, and the message says so.
	ring2, ring3 := charts.Ring2.Name, charts.Ring3.Name
	version := matched[ring2].version

	if version != matched[ring3].version {
		p.errf("error: incoherent chart pair in %s — the rings disagree on the version:\n", displayDir)
		p.errf("         ring2 %s: %s\n", ring2, version)
		p.errf("         ring3 %s: %s\n", ring3, matched[ring3].version)
		p.errf("       Both charts come out of ONE build and carry ONE version. Publishing this\n")
		p.errf("       pair would hand consumers a ring3 release paired with an infra chart it\n")
		p.errf("       was never packaged against.\n")
		p.errf("hint: rebuild with '%s'\n", rebuildHint)

		return errors.New("incoherent chart pair")
	}

	// The extras share the tag, so they share the version. One that does
	// not is a leftover from another build wearing this release's name.
	for _, ch := range charts.Extra {
		if matched[ch.Name].version == version {
			continue
		}

		p.errf("error: incoherent chart set in %s — %s does not carry the release version:\n", displayDir, ch.Name)
		p.errf("         rings:  %s\n", version)
		p.errf("         %s: %s\n", ch.Name, matched[ch.Name].version)
		p.errf("       Every chart of one tag is packaged from ONE manifest and carries ONE\n")
		p.errf("       version. This one came from somewhere else.\n")
		p.errf("hint: rebuild with '%s'\n", rebuildHint)

		return errors.New("incoherent chart set")
	}

	return nil
}

func (p *Pipeline) duplicateChartError(label, first, secondBase, displayDir, rebuildHint string) {
	p.errf("error: more than one %s chart in %s:\n", label, displayDir)
	p.errf("         %s\n", filepath.Base(first))
	p.errf("         %s.tgz\n", secondBase)
	p.errf("       One build produces exactly one tarball per chart. A leftover from an\n")
	p.errf("       earlier build makes the published version depend on glob order.\n")
	p.errf("hint: rebuild with '%s'\n", rebuildHint)
}

// verifyChartIdentity re-reads name and version from the tarball's own
// Chart.yaml and requires them to match what the filename claims — the
// selection keys on filenames, and a reused output directory can hold a
// file that merely LOOKS like a ring tarball.
func (p *Pipeline) verifyChartIdentity(tgz, expectedName, expectedVersion, displayDir, rebuildHint string) error {
	manifest, found, err := readChartYaml(tgz)
	if err != nil || !found {
		if !found {
			p.errf("error: %s in %s is not a packaged helm chart —\n", filepath.Base(tgz), displayDir)
			p.errf("       it has no Chart.yaml at the archive root.\n")
			p.errf("hint: rebuild with '%s'\n", rebuildHint)

			return fmt.Errorf("%s is not a packaged helm chart", filepath.Base(tgz))
		}

		p.errf("error: cannot read Chart.yaml out of %s in %s\n", filepath.Base(tgz), displayDir)
		p.errf("hint: rebuild with '%s'\n", rebuildHint)

		return fmt.Errorf("cannot read Chart.yaml out of %s: %w", filepath.Base(tgz), err)
	}

	if manifest.Name != expectedName || manifest.Version != expectedVersion {
		p.errf("error: chart tarball does not match its filename in %s:\n", displayDir)
		p.errf("         %s\n", filepath.Base(tgz))
		p.errf("       filename claims:  name '%s', version '%s'\n", expectedName, expectedVersion)
		p.errf("       Chart.yaml says:  name '%s', version '%s'\n", manifest.Name, manifest.Version)
		p.errf("       The filename is just a label — publishing this file would hand consumers\n")
		p.errf("       a chart the build never produced under that name.\n")
		p.errf("hint: rebuild with '%s'\n", rebuildHint)

		return fmt.Errorf("chart tarball %s does not match its filename", filepath.Base(tgz))
	}

	return nil
}

// chartIdentity is the pair of top-level scalars identity verification
// reads out of a packaged Chart.yaml.
type chartIdentity struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

// readChartYaml extracts <chart>/Chart.yaml (first match, archive root
// only) from a chart tarball. found is false when no such member exists
// or the file is not a gzipped tarball at all.
func readChartYaml(tgz string) (id chartIdentity, found bool, err error) {
	f, err := os.Open(tgz)
	if err != nil {
		return id, false, err
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return id, false, nil // not a gzipped archive → not a packaged chart
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return id, false, nil
		}

		if err != nil {
			return id, false, nil // torn archive → not a packaged chart
		}

		if hdr.Typeflag != tar.TypeReg || !chartYamlMember.MatchString(hdr.Name) {
			continue
		}

		raw, err := io.ReadAll(io.LimitReader(tr, maxChartYamlSize))
		if err != nil {
			return id, true, err
		}

		if err := yaml.Unmarshal(raw, &id); err != nil {
			return id, true, err
		}

		return id, true, nil
	}
}
