package conventions

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

const landingPage = "content/_index.md"

var navigationSuccessors = map[string]string{
	"1. Setup/1.1. Go.md":               "1. Setup/1.4. Providers.md",
	"1. Setup/1.5. Workspace.md":        "2. Agents/_index.md",
	"5. Gateway/5.0. Gateway.md":        "1. Setup/1.2. Container Engine.md",
	"1. Setup/1.2. Container Engine.md": "5. Gateway/5.1. Gateway Setup.md",
	"6. Platform/6.0. Platform.md":      "1. Setup/1.3. Kubernetes.md",
	"1. Setup/1.3. Kubernetes.md":       "6. Platform/6.1. Containers.md",
}

type pageSet map[string]string

func readFile(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func loadPages(root string) (pageSet, error) {
	contentRoot := filepath.Join(root, "content")
	pages := make(pageSet)
	err := filepath.WalkDir(contentRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		text, err := readFile(path)
		if err != nil {
			return err
		}
		pages[relative(root, path)] = text
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return pages, nil
	}
	return pages, err
}

// CheckDocs runs every authoring and source-owned course contract.
func CheckDocs(root string) []Problem {
	pages, err := loadPages(root)
	if err != nil {
		return []Problem{problem("content", "could not read Markdown pages: %v", err)}
	}
	if len(pages) == 0 {
		return []Problem{problem("content", "no Markdown pages found")}
	}
	paths := make([]string, 0, len(pages))
	for path := range pages {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	var problems []Problem
	for _, where := range paths {
		text := pages[where]
		path := filepath.Join(root, filepath.FromSlash(where))
		problems = append(problems, checkFrontMatter(where, text)...)
		problems = append(problems, checkPageURL(root, where, path, text)...)
		problems = append(problems, checkHeadings(where, text)...)
		problems = append(problems, checkSceneHeadings(where, text)...)
		problems = append(problems, checkHeadingOpeners(where, text)...)
		problems = append(problems, checkRetiredNarrative(where, text)...)
		problems = append(problems, checkGlance(where, text)...)
		problems = append(problems, checkCollapsibles(where, text)...)
		problems = append(problems, checkLinkLabels(where, text)...)
		problems = append(problems, checkSnippets(where, text)...)
		problems = append(problems, checkSnippetTargets(root, where, text)...)
		problems = append(problems, checkGoBlocks(where, text)...)
		problems = append(problems, checkMachinePaths(where, text)...)
		problems = append(problems, checkAdmonitions(where, text)...)
		problems = append(problems, checkOrderedLists(where, text)...)
		problems = append(problems, checkPageLinkTargets(where, text)...)
		problems = append(problems, checkHandsOnAction(where, text)...)
		problems = append(problems, checkExercises(where, text)...)
		problems = append(problems, checkDiagramAlternatives(where, text)...)
		problems = append(problems, checkImages(where, text)...)
		problems = append(problems, checkExactCountClaims(where, text)...)
		if where != landingPage {
			problems = append(problems, checkClosing(where, text)...)
			problems = append(problems, checkKind(where, text)...)
		}
	}
	// The corpus-level sequence lives in registry.go, where each entry carries a
	// name and declares whether the scheduled freshness report runs it too. The
	// list order is this function's execution order, unchanged.
	problems = append(problems, runChecks(corpusChecks, root, pages)...)
	return problems
}

func checkIndexKinds(pages pageSet) []Problem {
	pattern := regexp.MustCompile(`\[(\d+\.\d+\.[^]]+)\][^\n]*?_\(([a-z-]+)[^)]*\)_`)
	var problems []Problem
	for where, text := range pages {
		if filepath.Base(where) != "_index.md" {
			continue
		}
		for _, match := range pattern.FindAllStringSubmatch(text, -1) {
			target := filepath.ToSlash(filepath.Join(filepath.Dir(where), match[1]+".md"))
			declared := declaredKind(pages[target])
			if slices.Contains(pageKinds, match[2]) && declared != "" && match[2] != declared {
				problems = append(problems, problem(where, "labels %s as (%s) but that page declares (%s)", match[1], match[2], declared))
			}
		}
	}
	return problems
}

func navigationPaths(value any) []string {
	var result []string
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			result = append(result, navigationPaths(item)...)
		}
	case map[string]any:
		if page, ok := typed["page"].(string); ok {
			result = append(result, page)
		}
		result = append(result, navigationPaths(typed["children"])...)
	}
	return result
}

func checkNavigation(root string, pages pageSet) []Problem {
	const where = "data/nav.yaml"
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(where)))
	if err != nil {
		return []Problem{problem(where, "could not read navigation: %v", err)}
	}
	var entries any
	if err := yaml.Unmarshal(content, &entries); err != nil {
		return []Problem{problem(where, "could not read navigation: %v", err)}
	}
	if _, ok := entries.([]any); !ok {
		return []Problem{problem(where, "expected an explicit list of navigation entries")}
	}
	targets := navigationPaths(entries)
	counts := make(map[string]int)
	for _, target := range targets {
		counts[target]++
	}
	expected := make(map[string]bool)
	for path := range pages {
		expected[strings.TrimPrefix(path, "content/")] = true
	}
	actual := make(map[string]bool)
	var duplicates []string
	for target, count := range counts {
		actual[target] = true
		if count > 1 {
			duplicates = append(duplicates, target)
		}
	}
	slices.Sort(duplicates)
	missing := sortedDifference(expected, actual)
	extra := sortedDifference(actual, expected)
	var problems []Problem
	if len(duplicates) > 0 {
		problems = append(problems, problem(where, "navigation repeats pages: %s", strings.Join(duplicates, ", ")))
	}
	if len(missing) > 0 {
		problems = append(problems, problem(where, "navigation omits pages: %s", strings.Join(missing, ", ")))
	}
	if len(extra) > 0 {
		problems = append(problems, problem(where, "navigation references unknown pages: %s", strings.Join(extra, ", ")))
	}
	positions := make(map[string]int)
	for index, target := range targets {
		positions[target] = index
	}
	for current, successor := range navigationSuccessors {
		position, ok := positions[current]
		if !ok || position+1 >= len(targets) || targets[position+1] != successor {
			problems = append(problems, problem(where, "navigation after %s must continue to %s", current, successor))
		}
	}
	return problems
}

func checkCostOwner(pages pageSet) []Problem {
	owner := "content/7. Observability/7.3. Costs.md"
	currency := regexp.MustCompile(`\bUSD\s+\d|\$\s*\d+(?:\.\d+)?\s*(?:per|/)\s*(?:hour|month)`)
	date := regexp.MustCompile(`\bprices? checked on \d{1,2} [A-Z][a-z]+ \d{4}\b`)
	var problems []Problem
	for where, text := range pages {
		if where == owner {
			continue
		}
		if currency.MatchString(text) {
			problems = append(problems, problem(where, "GKE currency literals belong only in 7.3. Costs; link to its dated estimate"))
		}
		if date.MatchString(text) {
			problems = append(problems, problem(where, "dated price review belongs only in 7.3. Costs"))
		}
	}
	return problems
}

func checkGCPRunbook(root string) []Problem {
	const where = "infra/gcp/README.md"
	text, err := readFile(filepath.Join(root, filepath.FromSlash(where)))
	if err != nil {
		return []Problem{problem(where, "could not read GCP runbook: %v", err)}
	}
	pattern := regexp.MustCompile(`\btofu\s+`)
	var problems []Problem
	for index, line := range splitLines(text) {
		if pattern.MatchString(line) && !strings.Contains(line, "-chdir=infra/gcp") {
			problems = append(problems, problem(where, "line %d: root-scoped OpenTofu command needs `-chdir=infra/gcp`", index+1))
		}
	}
	return problems
}

func checkSkaffoldRunbooks(root string) []Problem {
	badProfile := regexp.MustCompile(`\bskaffold\s+(?:run|delete)\s+-p\s+(?:local|gke)\b`)
	var problems []Problem
	// The corpus is the course pages and the infrastructure runbooks, which is where a
	// learner copies a Skaffold command from. This walked "docs" until now — the MkDocs
	// source directory the Python course kept its pages in, which has never existed in
	// this repository — so no course page was ever covered by the rule.
	for _, directory := range []string{"content", "infra"} {
		base := filepath.Join(root, directory)
		_ = filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || filepath.Ext(path) != ".md" {
				return nil
			}
			text, readErr := readFile(path)
			if readErr != nil {
				return nil
			}
			for index, line := range splitLines(text) {
				if strings.Contains(line, "--filename infra/skaffold.yaml") || badProfile.MatchString(line) {
					problems = append(problems, problem(relative(root, path), "line %d: run from `infra/` with `--filename skaffold.yaml`", index+1))
				}
			}
			return nil
		})
	}
	return problems
}

func formatPorts(values map[int]bool) string {
	ports := make([]int, 0, len(values))
	for port := range values {
		ports = append(ports, port)
	}
	slices.Sort(ports)
	parts := make([]string, len(ports))
	for index, port := range ports {
		parts[index] = fmt.Sprintf(":%d", port)
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
