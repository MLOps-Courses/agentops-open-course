package conventions

// The committed capture manifest. Suite totals and per-package coverage cannot be
// derived without running the suites, so check:docs reads what docs:captures wrote.

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type captureSuite struct {
	Tests   int `yaml:"tests"`
	Skipped int `yaml:"skipped"`
}

type captureManifest struct {
	Suites   map[string]captureSuite `yaml:"suites"`
	Coverage map[string]float64      `yaml:"coverage"`
}

var (
	doneTests      = regexp.MustCompile(`DONE ([0-9]+) tests(?:, ([0-9]+) skipped)? in `)
	coverageReport = regexp.MustCompile(`\bok\s+([0-9]+\.[0-9]+)%\s+(agents/go/\S+)`)
	coverageTick   = regexp.MustCompile(`✓\s+(\S+)\s+\([^)]*\)\s+\(coverage:\s+([0-9]+\.[0-9]+)% of statements\)`)
)

func checkCaptureManifest(root string, pages pageSet) []Problem {
	const where = "data/captures.yaml"
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(where)))
	if err != nil {
		return []Problem{problem(where, "could not read the capture manifest; run `mise run docs:captures`: %v", err)}
	}
	var manifest captureManifest
	if err := yaml.Unmarshal(content, &manifest); err != nil {
		return []Problem{problem(where, "could not read the capture manifest: %v", err)}
	}
	if len(manifest.Suites) == 0 || len(manifest.Coverage) == 0 {
		return []Problem{problem(where, "capture manifest must declare suite totals and per-package coverage")}
	}
	summaries := make(map[[2]int]bool, len(manifest.Suites))
	for _, suite := range manifest.Suites {
		summaries[[2]int{suite.Tests, suite.Skipped}] = true
	}
	var problems []Problem
	for page, text := range pages {
		for _, line := range splitLines(text) {
			problems = append(problems, checkCaptureLine(page, line, summaries, manifest.Coverage)...)
		}
	}
	slices.SortFunc(problems, func(left, right Problem) int { return strings.Compare(left.String(), right.String()) })
	return problems
}

func checkCaptureLine(page, line string, summaries map[[2]int]bool, coverage map[string]float64) []Problem {
	var problems []Problem
	for _, match := range doneTests.FindAllStringSubmatch(line, -1) {
		tests, _ := strconv.Atoi(match[1])
		skipped, _ := strconv.Atoi(match[2])
		if !summaries[[2]int{tests, skipped}] {
			problems = append(problems, problem(page, "capture claims %s tests / %d skipped, which no suite in data/captures.yaml reports", match[1], skipped))
		}
	}
	for _, match := range coverageReport.FindAllStringSubmatch(line, -1) {
		problems = append(problems, comparePackageCoverage(page, match[2], match[1], coverage)...)
	}
	for _, match := range coverageTick.FindAllStringSubmatch(line, -1) {
		// gotestsum prints the bare package name; every such capture in content/ is
		// from the agent suite, so an unknown name is another module's and is skipped.
		problems = append(problems, comparePackageCoverage(page, "agents/go/"+match[1], match[2], coverage)...)
	}
	return problems
}

func comparePackageCoverage(page, packagePath, printed string, coverage map[string]float64) []Problem {
	expected, known := coverage[packagePath]
	if !known {
		return nil
	}
	if printed != strconv.FormatFloat(expected, 'f', 1, 64) {
		return []Problem{problem(page, "capture reports %s at %s%%; data/captures.yaml records %.1f%%", packagePath, printed, expected)}
	}
	return nil
}
