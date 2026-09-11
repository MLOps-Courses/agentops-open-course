package conventions

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var inlineCodePattern = regexp.MustCompile("`([^`\\n]+)`")

var repositoryPathRoots = []string{
	".github/",
	"agents/",
	"assets/",
	"clients/",
	"content/",
	"data/",
	"evals/",
	"infra/",
	"layouts/",
	"load/",
	"scripts/",
	"skills/",
	"static/",
	"tools/",
}

var generatedPathPrefixes = []string{
	"agents/go/.state",
	"agents/go/bin",
	"assets/source",
	// The canonical run artifact plus the three a task can still produce. Any other
	// name comes from an explicit --output, which a page should not be quoting.
	"evals/results.json",
	"evals/judge-calibration-results.json",
	"evals/prompt-comparison.json",
	"evals/retrieval-results.json",
	"infra/agentgateway/host/auth/",
	// 5.5's config-authoring exercise: the learner writes this from an empty file,
	// and it is git-ignored so a half-finished attempt leaves nothing to explain.
	"infra/agentgateway/lab/",
	"infra/k8s/base/secrets/my-secret.enc.yaml",
	"infra/secrets",
	"site/",
	"tools/bin/",
}

func repositoryPathFromCode(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "<>*${}\n\t") || strings.Contains(value, "...") {
		return ""
	}
	value = strings.Trim(value, "'\"(),;:<>")
	value = strings.TrimPrefix(value, "./")
	for _, root := range repositoryPathRoots {
		if strings.HasPrefix(value, root) {
			return strings.TrimSuffix(value, "/")
		}
	}
	return ""
}

func generatedRepositoryPath(path string) bool {
	for _, prefix := range generatedPathPrefixes {
		root := strings.TrimSuffix(prefix, "/")
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

func checkLiteralRepositoryPath(root, where string, line int, path string) []Problem {
	if path == "" || generatedRepositoryPath(path) {
		return nil
	}
	if strings.Contains(path, " ") {
		return nil
	}
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); err == nil {
		return nil
	}
	if _, err := os.Lstat(filepath.Join(root, "agents", "go", filepath.FromSlash(path))); err == nil {
		return nil
	}
	// MCP's protocol methods are written like relative paths but are not
	// repository files. Keep these exact so missing tools/* files still fail.
	if path == "tools/call" || path == "tools/list" {
		return nil
	}
	return []Problem{problem(where, "line %d: literal repository path does not exist: %s", line, path)}
}

func repositoryPathFromShellToken(token string) string {
	if _, value, found := strings.Cut(token, "="); found {
		token = value
	}
	return repositoryPathFromCode(token)
}

func checkLiteralRepositoryPaths(root string, pages pageSet) []Problem {
	paths := make([]string, 0, len(pages))
	for where := range pages {
		paths = append(paths, where)
	}
	slices.Sort(paths)
	var problems []Problem
	for _, where := range paths {
		for _, line := range linesOutsideFences(pages[where]) {
			for _, match := range inlineCodePattern.FindAllStringSubmatch(line.text, -1) {
				path := repositoryPathFromCode(match[1])
				problems = append(problems, checkLiteralRepositoryPath(root, where, line.number, path)...)
			}
		}
		for _, language := range []string{"bash", "console", "sh", "shell"} {
			for _, block := range fencedBlocks(pages[where], language) {
				for index, line := range block.body {
					for _, token := range strings.Fields(line) {
						path := repositoryPathFromShellToken(token)
						problems = append(problems, checkLiteralRepositoryPath(root, where, block.start+index+1, path)...)
					}
				}
			}
		}
	}
	return problems
}

func shellCommands(lines []string) []string {
	commands := make([]string, 0, len(lines))
	var pending string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		continued := strings.HasSuffix(trimmed, "\\")
		trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, "\\"))
		if pending != "" {
			pending += " "
		}
		pending += trimmed
		if !continued {
			commands = append(commands, pending)
			pending = ""
		}
	}
	if pending != "" {
		commands = append(commands, pending)
	}
	return commands
}

func localGoArgument(token string) (string, bool) {
	token = strings.Trim(token, "'\"(),;\\")
	if strings.HasPrefix(token, "./") || strings.HasPrefix(token, "../") || strings.HasSuffix(token, ".go") {
		return token, true
	}
	return "", false
}

func validateGoArgument(root, directory, argument string) string {
	path := argument
	if strings.HasSuffix(path, "/...") {
		path = strings.TrimSuffix(path, "...")
	}
	target := filepath.Clean(filepath.Join(directory, filepath.FromSlash(path)))
	if !within(root, target) {
		return "escapes the repository"
	}
	info, err := os.Stat(target)
	if err != nil {
		return "does not exist"
	}
	if strings.HasSuffix(argument, ".go") {
		if !info.Mode().IsRegular() {
			return "is not a Go file"
		}
		return ""
	}
	if !info.IsDir() {
		return "is not a Go package directory"
	}
	if strings.HasSuffix(argument, "/...") {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(target, "*.go"))
	if len(matches) == 0 {
		return "contains no Go package"
	}
	return ""
}

func checkGoTestCommand(root, where string, line int, directory *string, command string) []Problem {
	var problems []Problem
	segments := regexp.MustCompile(`\s*(?:&&|;)\s*`).Split(command, -1)
	for _, segment := range segments {
		fields := strings.Fields(segment)
		if len(fields) >= 2 && fields[0] == "cd" {
			target := strings.Trim(fields[1], "'\"")
			candidate := filepath.Clean(filepath.Join(*directory, filepath.FromSlash(target)))
			if within(root, candidate) {
				*directory = candidate
			}
			continue
		}
		start := -1
		for index := 0; index+1 < len(fields); index++ {
			if fields[index] == "go" && fields[index+1] == "test" {
				start = index + 2
				break
			}
		}
		if start < 0 {
			continue
		}
		for _, token := range fields[start:] {
			argument, local := localGoArgument(token)
			if !local {
				continue
			}
			if message := validateGoArgument(root, *directory, argument); message != "" {
				problems = append(problems, problem(where, "line %d: documented go test argument %q %s from %s", line, argument, message, relative(root, *directory)))
			}
		}
	}
	return problems
}

func checkDocumentedGoPackages(root string, pages pageSet) []Problem {
	paths := make([]string, 0, len(pages))
	for where := range pages {
		paths = append(paths, where)
	}
	slices.Sort(paths)
	var problems []Problem
	for _, where := range paths {
		for _, line := range linesOutsideFences(pages[where]) {
			for _, match := range inlineCodePattern.FindAllStringSubmatch(line.text, -1) {
				if !strings.Contains(match[1], "go test") {
					continue
				}
				directory := root
				problems = append(problems, checkGoTestCommand(root, where, line.number, &directory, match[1])...)
			}
		}
		for _, language := range []string{"bash", "console", "sh", "shell"} {
			for _, block := range fencedBlocks(pages[where], language) {
				directory := root
				for index, command := range shellCommands(block.body) {
					problems = append(problems, checkGoTestCommand(root, where, block.start+index+1, &directory, command)...)
				}
			}
		}
	}
	return problems
}
