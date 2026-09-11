package conventions

// Repository identity. The published slug lives here, together with the scan that
// bans the retired name and the check that proves runbook links resolve.

import (
	"bytes"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// RepositorySlug is the one place the published repository is named. The rendered
// edit link, the runbook annotations, and the site navbar all resolve to it, and
// checkRepositorySlug rejects the staging slug this course was written under.
const RepositorySlug = "MLOps-Courses/agentops-open-course"

// staleRepositorySlug is the address the Go rewrite carried while it lived in its own
// checkout, before it replaced the Python course at RepositorySlug. That repository was
// never created, so any surviving reference is a dead link. It is derived rather than
// spelled so this file does not contain the string the check bans and flag its own source.
var staleRepositorySlug = RepositorySlug + "-go"

// Build output, agent scratch space, and git internals legitimately carry the retired
// name — site/ is a rebuild of whatever content said at the time, and .agents/ holds
// the work orders that describe the migration.
var slugSkippedDirectories = map[string]bool{
	".agents": true, ".git": true, "node_modules": true, "resources": true, "site": true,
}

// slugScanLimit keeps the walk off the repository's few large binaries — the seeded
// SQLite database and the vendored sqlite3 build — which cannot hold a URL anyway.
const slugScanLimit = 2 << 20

func isBinary(content []byte) bool {
	head := content
	if len(head) > 8192 {
		head = head[:8192]
	}
	return bytes.IndexByte(head, 0) >= 0
}

// checkRepositorySlug bans the retired repository name and proves every runbook
// annotation resolves to a page that exists.
//
// `check:links` runs lychee offline, which validates repository-local paths and never
// resolves a GitHub URL — so eighteen dead links passed every gate. The second rule is
// the one that catches an annotation pointing at a course page in a directory layout
// (`docs/`) this repository has never had.
func checkRepositorySlug(root string) []Problem {
	// Root-scoped, so a symlink inside the tree cannot walk the check out of it.
	scope, err := os.OpenRoot(root)
	if err != nil {
		return []Problem{problem(".", "could not open the repository: %v", err)}
	}
	defer func() { _ = scope.Close() }()
	var problems []Problem
	walkErr := fs.WalkDir(scope.FS(), ".", func(where string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if slugSkippedDirectories[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() > slugScanLimit {
			return nil
		}
		content, err := scope.ReadFile(where)
		if err != nil || isBinary(content) {
			return nil
		}
		problems = append(problems, retiredSlugProblems(where, string(content))...)
		return nil
	})
	if walkErr != nil {
		problems = append(problems, problem(".", "could not walk the repository: %v", walkErr))
	}
	return append(problems, checkRunbookTargets(root)...)
}

func retiredSlugProblems(where, content string) []Problem {
	// GitHub resolves owner and repository names case-insensitively, so an all-lowercase
	// copy of the stale slug addresses the same dead repository as the mixed-case
	// spelling. (The lowercase form is not written out here for the same reason the slug
	// itself is derived above: this file would then flag its own source.) Both the needle
	// and each line are lowered so the scan catches every casing; only the line number is
	// reported, so byte offsets never have to survive the fold.
	needle := strings.ToLower(staleRepositorySlug)
	var problems []Problem
	for index, line := range splitLines(content) {
		if strings.Contains(strings.ToLower(line), needle) {
			problems = append(problems, problem(where, "line %d: names the staging repository; use %s", index+1, RepositorySlug))
		}
	}
	return problems
}

func checkRunbookTargets(root string) []Problem {
	const where = "infra/k8s/overlays/local/prometheus-rules.yaml"
	text, err := readFile(filepath.Join(root, filepath.FromSlash(where)))
	if err != nil {
		return []Problem{problem(where, "could not read alerting rules: %v", err)}
	}
	var problems []Problem
	for _, match := range regexp.MustCompile(`(?m)^\s*runbook:\s*(\S+)$`).FindAllStringSubmatch(text, -1) {
		target, _, _ := strings.Cut(match[1], "#")
		_, blob, found := strings.Cut(target, "/blob/main/")
		if !found {
			problems = append(problems, problem(where, "runbook %q must address a file on the repository's main branch", match[1]))
			continue
		}
		decoded, err := url.PathUnescape(blob)
		if err != nil {
			problems = append(problems, problem(where, "runbook %q is not a decodable path", match[1]))
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(decoded))); err != nil {
			problems = append(problems, problem(where, "runbook %q points at a page this repository does not hold", decoded))
		}
	}
	return problems
}
