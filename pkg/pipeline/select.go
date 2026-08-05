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

// selection is a proven-coherent ring pair: exactly one tarball per
// ring, both rings on the one version a single build produced.
type selection struct {
	InfraTgz string // ring2
	AppTgz   string // ring3
	Version  string
}

// chartYamlMember matches helm's packing layout: <chart>/Chart.yaml at
// the archive root (the scripts' `grep -x '[^/]*/Chart\.yaml'`).
var chartYamlMember = regexp.MustCompile(`^[^/]*/Chart\.yaml$`)

// maxChartYamlSize bounds how much of a tarball member is read while
// verifying identity — Chart.yaml is a few hundred bytes.
const maxChartYamlSize = 1 << 20

// selectRingCharts is the port of charts-select.sh: pick the ring2 +
// ring3 tarballs out of a packaged charts directory and prove the pair
// is COHERENT. Used by BOTH push paths so the two cannot drift apart.
//
// The charts output is a REUSED build directory, not a fresh temp dir,
// so "whatever .tgz files happen to be there" is not a safe input:
//
//   - two tarballs for the same ring — an older version left behind by a
//     partially cleaned build — would silently publish whichever one a
//     glob happened to match last;
//   - a ring2 and a ring3 tarball from DIFFERENT builds would publish an
//     incoherent pair claiming two different release versions;
//   - a file merely NAMED like a ring tarball would be published under a
//     name and version the chart inside it does not carry.
//
// chartsDir is the directory actually read — for both push paths a
// private staged snapshot. displayDir is the directory NAMED in error
// messages: naming the transient stage would point the operator at a
// path deleted the moment the flow exits, while the rebuild that fixes
// the problem happens in the REAL output dir.
//
// Note the ordered matching: the ring3 prefix (<ring3>-*) also matches
// the ring2 tarball, so ring2 must be matched FIRST.
func (p *Pipeline) selectRingCharts(chartsDir, rebuildHint, displayDir string) (*selection, error) {
	ring2 := p.cfg.Charts.Ring2.Name
	ring3 := p.cfg.Charts.Ring3.Name

	entries, err := os.ReadDir(chartsDir)
	if err != nil {
		return nil, fmt.Errorf("read charts dir: %w", err)
	}

	var infraTgz, appTgz, infraVersion, appVersion string

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

		full := filepath.Join(chartsDir, name)
		base := strings.TrimSuffix(name, ".tgz")

		switch {
		case strings.HasPrefix(base, ring2+"-"):
			if infraTgz != "" {
				p.duplicateRingError("ring2", infraTgz, base, displayDir, rebuildHint)

				return nil, errors.New("more than one ring2 chart")
			}

			infraTgz = full
			infraVersion = strings.TrimPrefix(base, ring2+"-")
		case strings.HasPrefix(base, ring3+"-"):
			if appTgz != "" {
				p.duplicateRingError("ring3", appTgz, base, displayDir, rebuildHint)

				return nil, errors.New("more than one ring3 chart")
			}

			appTgz = full
			appVersion = strings.TrimPrefix(base, ring3+"-")
		default:
			p.errf("error: unexpected chart tgz '%s' in %s\n", base, displayDir)
			p.errf("hint: rebuild with '%s'\n", rebuildHint)

			return nil, fmt.Errorf("unexpected chart tgz %s", base)
		}
	}

	// Both rings must be present — a partially packaged directory must
	// never publish half a build.
	var missing []string
	if infraTgz == "" {
		missing = append(missing, fmt.Sprintf("%s-*.tgz (ring2)", ring2))
	}

	if appTgz == "" {
		missing = append(missing, fmt.Sprintf("%s-*.tgz (ring3)", ring3))
	}

	if len(missing) > 0 {
		p.errf("error: incomplete chart set in %s — missing: %s\n", displayDir, strings.Join(missing, " "))
		p.errf("hint: rebuild with '%s'\n", rebuildHint)

		return nil, errors.New("incomplete chart set")
	}

	if infraVersion == "" || appVersion == "" {
		p.errf("error: unversioned chart tarball in %s\n", displayDir)
		p.errf("       ring2: %s\n", filepath.Base(infraTgz))
		p.errf("       ring3: %s\n", filepath.Base(appTgz))
		p.errf("hint: rebuild with '%s'\n", rebuildHint)

		return nil, errors.New("unversioned chart tarball")
	}

	// The rings are packaged together from one manifest version.
	// Different versions mean the directory mixes two builds.
	if infraVersion != appVersion {
		p.errf("error: incoherent chart pair in %s — the rings disagree on the version:\n", displayDir)
		p.errf("         ring2 %s: %s\n", ring2, infraVersion)
		p.errf("         ring3 %s: %s\n", ring3, appVersion)
		p.errf("       Both charts come out of ONE build and carry ONE version. Publishing this\n")
		p.errf("       pair would hand consumers a ring3 release paired with an infra chart it\n")
		p.errf("       was never packaged against.\n")
		p.errf("hint: rebuild with '%s'\n", rebuildHint)

		return nil, errors.New("incoherent chart pair")
	}

	// Every check above trusted FILENAMES, and a file can lie. Prove each
	// selected tarball actually contains the chart its name claims before
	// anything downstream treats it as one.
	if err := p.verifyChartIdentity(infraTgz, ring2, infraVersion, displayDir, rebuildHint); err != nil {
		return nil, err
	}

	if err := p.verifyChartIdentity(appTgz, ring3, appVersion, displayDir, rebuildHint); err != nil {
		return nil, err
	}

	return &selection{InfraTgz: infraTgz, AppTgz: appTgz, Version: infraVersion}, nil
}

func (p *Pipeline) duplicateRingError(ring, first, secondBase, displayDir, rebuildHint string) {
	p.errf("error: more than one %s chart in %s:\n", ring, displayDir)
	p.errf("         %s\n", filepath.Base(first))
	p.errf("         %s.tgz\n", secondBase)
	p.errf("       One build produces exactly one tarball per ring. A leftover from an\n")
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
