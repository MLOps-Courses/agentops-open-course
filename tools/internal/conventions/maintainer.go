package conventions

// Maintainer surfaces. Quickstarts, hooks, doctor tiers, and the custom metric
// inventory drift the moment a script changes and its page does not.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

func containsInOrder(text string, values ...string) bool {
	position := 0
	for _, value := range values {
		found := strings.Index(text[position:], value)
		if found < 0 {
			return false
		}
		position += found + len(value)
	}
	return true
}

func checkQuickstarts(root string, pages pageSet) []Problem {
	// The clone leads, because every command after it resolves a path inside the
	// checkout: `mise trust mise.toml` on a page that never said to clone is a
	// relative path with nothing to resolve against.
	sequence := []string{
		"git clone https://github.com/" + RepositorySlug,
		"mise run install", "mise run doctor", "mise run check:core", "mise run test",
	}
	const systemWhere = "content/1. Setup/1.0. System.md"
	readme, _ := readFile(filepath.Join(root, "README.md"))
	var problems []Problem
	for where, text := range map[string]string{"README.md": readme, systemWhere: pages[systemWhere]} {
		if !containsInOrder(text, sequence...) {
			problems = append(problems, problem(where, "guarded quickstart must contain the canonical clone → install → doctor → check → test sequence"))
		}
	}
	// `mise run install` builds SQLite from source, so a learner without a compiler
	// hits scripts/install-sqlite.sh's `require_host_cmd cc` on the first of the four
	// green-path commands. The page that owns the prerequisite has to be actionable.
	for _, command := range []string{"build-essential", "development-tools", "xcode-select --install"} {
		if !strings.Contains(pages[systemWhere], command) {
			problems = append(problems, problem(systemWhere, "the C-toolchain prerequisite must name the %q host command", command))
		}
	}
	landing := pages[landingPage]
	if !strings.Contains(landing, "<!-- quickstart: unverified-preview -->") || !strings.Contains(strings.ToLower(landing), "unverified preview") {
		problems = append(problems, problem(landingPage, "shorter model-first quickstart must be marked as an unverified preview"))
	}
	ci, _ := readFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if !containsInOrder(ci, "run: mise run install:validation", "run: mise run doctor", "run: mise run check", "run: mise run test") {
		problems = append(problems, problem(".github/workflows/ci.yml", "CI must execute install, the learner base doctor, check, and test in order"))
	}
	linting := pages["content/4. Quality/4.1. Linting.md"]
	if strings.Count(linting, "install:validation") != 1 ||
		!strings.Contains(linting, "documentation workflow installs only its pinned documentation and browser toolchains") ||
		strings.Contains(linting, "install:maintainer") {
		problems = append(problems, problem("content/4. Quality/4.1. Linting.md", "CI prose must distinguish the validation installer from the documentation workflow's narrower toolchain"))
	}
	return problems
}

func hookTaskContract(text string) string {
	before, after, ok := strings.Cut(text, "\npre-push:\n")
	if !ok {
		return ""
	}
	pattern := regexp.MustCompile(`(?m)^\s+run: mise run ([^\s{]+)`)
	collect := func(value string) string {
		matches := pattern.FindAllStringSubmatch(value, -1)
		names := make([]string, 0, len(matches))
		for _, match := range matches {
			names = append(names, match[1])
		}
		return strings.Join(names, ",")
	}
	return "pre-commit=" + collect(before) + "; pre-push=" + collect(after)
}

// doctorTiers maps each profile bullet on the System page onto the scripts/doctor.sh
// arrays that profile adds.
//
// The page documents deltas — "each heavier profile adds its own tier on top" — so
// every entry lists what one `add_*_tier` function contributes, not the cumulative
// list a profile ends up checking: `model` runs the base tier and then adds
// `model_host_tools`, and both `platform` and `gcp` run the base and gateway tiers
// before adding their own.
var doctorTiers = []struct {
	name   string
	arrays []string
}{
	{"base", []string{"base_managed_tools", "base_host_tools"}},
	{"model", []string{"model_host_tools"}},
	{"gateway", []string{"gateway_managed_tools", "gateway_host_tools"}},
	{"platform", []string{"platform_tools"}},
	{"gcp", []string{"gcp_platform_tools", "gcp_host_tools"}},
}

func doctorToolArrays(text string) map[string]map[string]bool {
	result := make(map[string]map[string]bool)
	pattern := regexp.MustCompile(`(?ms)^readonly -a (\w+)=\((.*?)\)$`)
	for _, match := range pattern.FindAllStringSubmatch(text, -1) {
		set := make(map[string]bool)
		for _, value := range strings.Fields(match[2]) {
			set[value] = true
		}
		result[match[1]] = set
	}
	return result
}

func sourceMetrics(root string) map[string]bool {
	metrics := make(map[string]bool)
	_ = filepath.WalkDir(filepath.Join(root, "agents", "go"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			return nil
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.ValueSpec:
				for index, name := range typed.Names {
					if !strings.HasSuffix(name.Name, "Metric") || index >= len(typed.Values) {
						continue
					}
					if literal, ok := typed.Values[index].(*ast.BasicLit); ok && literal.Kind == token.STRING {
						if value, unquoteErr := strconv.Unquote(literal.Value); unquoteErr == nil && strings.HasPrefix(value, "agentops.") {
							metrics[value] = true
						}
					}
				}
			case *ast.CallExpr:
				selector, ok := typed.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "Int64Counter" || len(typed.Args) == 0 {
					return true
				}
				if literal, ok := typed.Args[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
					if value, unquoteErr := strconv.Unquote(literal.Value); unquoteErr == nil && strings.HasPrefix(value, "agentops.") {
						metrics[value] = true
					}
				}
			}
			return true
		})
		return nil
	})
	return metrics
}

func checkMaintainerDrift(root string, pages pageSet) []Problem {
	var problems []Problem
	hook, _ := readFile(filepath.Join(root, "lefthook.yml"))
	marker := "<!-- lefthook-tasks: " + hookTaskContract(hook) + " -->"
	for _, where := range []string{"content/1. Setup/1.5. Workspace.md", "content/4. Quality/4.1. Linting.md"} {
		if !strings.Contains(pages[where], marker) {
			problems = append(problems, problem(where, "lefthook task description drifted from lefthook.yml"))
		}
	}
	doctor, _ := readFile(filepath.Join(root, "scripts", "doctor.sh"))
	const systemWhere = "content/1. Setup/1.0. System.md"
	system := pages[systemWhere]
	if !strings.Contains(system, `{{< include path="scripts/doctor.sh" region="doctor-tool-tiers"`) {
		problems = append(problems, problem(systemWhere, "doctor tool tiers must include the source-owned arrays"))
	}
	arrays := doctorToolArrays(doctor)
	// Resolve every tier through the arrays the script really declares, and report a
	// name that is not there instead of comparing the prose against a nil set. Four of
	// these lookups once named arrays a rename had already retired, so the base, model,
	// and gateway bullets compared against nothing and could say anything at all.
	expected := make(map[string]map[string]bool, len(doctorTiers))
	consumed := make(map[string]bool, len(doctorTiers))
	for _, tier := range doctorTiers {
		wanted := make(map[string]bool)
		resolved := true
		for _, name := range tier.arrays {
			consumed[name] = true
			set, ok := arrays[name]
			if !ok {
				problems = append(problems, problem("scripts/doctor.sh",
					"doctor %s tier reads %s, which the script no longer declares", tier.name, name))
				resolved = false
				continue
			}
			maps.Copy(wanted, set)
		}
		// A tier whose arrays could not be resolved is left uncompared: the missing array
		// is already reported, and grading the prose against a partial set would only add
		// a second, misleading drift message.
		if resolved {
			expected[tier.name] = wanted
		}
	}
	// The same hole from the other side: a tool array the script declares that no tier
	// consumes is a profile this page never documents.
	for _, name := range slices.Sorted(maps.Keys(arrays)) {
		if strings.HasSuffix(name, "_tools") && !consumed[name] {
			problems = append(problems, problem("scripts/doctor.sh",
				"doctor array %s belongs to no documented tier", name))
		}
	}
	known := make(map[string]bool)
	for _, set := range expected {
		for value := range set {
			known[value] = true
		}
	}
	for _, tier := range doctorTiers {
		wanted, ok := expected[tier.name]
		if !ok {
			continue
		}
		prefix := "- **" + tier.name + "**"
		if tier.name == "base" {
			prefix = "The base doctor checks"
		}
		line := ""
		for _, candidate := range splitLines(system) {
			if strings.HasPrefix(strings.TrimLeft(candidate, " \t"), prefix) {
				line = candidate
				break
			}
		}
		documented := make(map[string]bool)
		for _, match := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(line, -1) {
			if known[match[1]] {
				documented[match[1]] = true
			}
		}
		if !mapsEqual(wanted, documented) {
			problems = append(problems, problem(systemWhere, "doctor %s tool list drifted: expected %v, found %v", tier.name, sortedKeys(wanted), sortedKeys(documented)))
		}
	}
	workflow, _ := readFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	match := regexp.MustCompile(`(?m)^\s+run: mise run (install:[\w-]+)\s*$`).FindStringSubmatch(workflow)
	if match == nil {
		problems = append(problems, problem(".github/workflows/ci.yml", "CI must declare one scoped install task"))
	} else {
		for _, where := range []string{"content/1. Setup/1.5. Workspace.md", "content/4. Quality/4.1. Linting.md", "content/8. Community/8.5. Contributions.md"} {
			if !strings.Contains(pages[where], match[1]) {
				problems = append(problems, problem(where, "CI prose must name `%s` from ci.yml", match[1]))
			}
		}
	}
	const metricsWhere = "content/4. Quality/4.3. Metrics.md"
	section, missing := sectionAfterHeading(metricsWhere, pages[metricsWhere], "## Which application metrics ship?")
	if len(missing) > 0 {
		return append(problems, missing...)
	}
	documented := make(map[string]bool)
	for _, match := range regexp.MustCompile("`(agentops\\.[^`]+)`").FindAllStringSubmatch(section, -1) {
		documented[match[1]] = true
	}
	expectedMetrics := sourceMetrics(root)
	if !mapsEqual(expectedMetrics, documented) {
		problems = append(problems, problem(metricsWhere, "custom metric inventory drifted: expected %v, found %v", sortedKeys(expectedMetrics), sortedKeys(documented)))
	}
	return problems
}
