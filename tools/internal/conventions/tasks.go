package conventions

// Task manifests. These checks read mise.toml files as the authority for what a
// task expands to, what it depends on, and what an install task may do, so a page
// or a workflow cannot document a task vocabulary the manifests do not declare.

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml"
)

func loadTOML(path string) (*toml.Tree, error) {
	return toml.LoadFile(path)
}

func taskNames(path string) map[string]bool {
	document, err := loadTOML(path)
	if err != nil {
		return nil
	}
	tasks, ok := document.Get("tasks").(*toml.Tree)
	if !ok {
		return nil
	}
	result := make(map[string]bool)
	for _, key := range tasks.Keys() {
		result[key] = true
	}
	return result
}

func scalarString(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case int64:
		return strconv.FormatInt(typed, 10), true
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64), true
	case bool:
		return strconv.FormatBool(typed), true
	default:
		return "", false
	}
}

func taskExpansion(path, name string) string {
	document, err := loadTOML(path)
	if err != nil {
		return ""
	}
	task, ok := document.GetPath([]string{"tasks", name}).(*toml.Tree)
	if !ok {
		return ""
	}
	run, ok := task.Get("run").(string)
	if !ok {
		return ""
	}
	var prefixes []string
	if environment, ok := task.Get("env").(*toml.Tree); ok {
		keys := environment.Keys()
		slices.Sort(keys)
		for _, key := range keys {
			if key != strings.ToUpper(key) {
				continue
			}
			if value, scalar := scalarString(environment.Get(key)); scalar {
				prefixes = append(prefixes, key+"="+value)
			}
		}
	}
	return strings.Join(append(prefixes, run), " ")
}

func taskDependencies(path, name string) []string {
	document, err := loadTOML(path)
	if err != nil {
		return nil
	}
	task, ok := document.GetPath([]string{"tasks", name}).(*toml.Tree)
	if !ok {
		return nil
	}
	switch values := task.Get("depends").(type) {
	case []string:
		return values
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if dependency, stringValue := value.(string); stringValue {
				result = append(result, dependency)
			}
		}
		return result
	default:
		return nil
	}
}

func taskCommands(path, name string) []string {
	document, err := loadTOML(path)
	if err != nil {
		return nil
	}
	task, ok := document.GetPath([]string{"tasks", name}).(*toml.Tree)
	if !ok {
		return nil
	}
	switch values := task.Get("run").(type) {
	case string:
		return []string{values}
	case []string:
		return values
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if command, stringValue := value.(string); stringValue {
				result = append(result, command)
			}
		}
		return result
	default:
		return nil
	}
}

func checkSerializedModuleChecks(root string) []Problem {
	manifest := filepath.Join(root, "mise.toml")
	sourceDependencies := taskDependencies(manifest, "check:source")
	for _, direct := range []string{"check:go", "check:evals", "check:tools"} {
		if slices.Contains(sourceDependencies, direct) {
			return []Problem{problem("mise.toml", "check:source must serialize Go module linters through check:modules")}
		}
	}
	if !slices.Contains(sourceDependencies, "check:modules") {
		return []Problem{problem("mise.toml", "check:source must depend on serialized check:modules")}
	}
	wanted := []string{"mise run check:go", "mise run check:evals", "mise run check:tools"}
	if !slices.Equal(taskCommands(manifest, "check:modules"), wanted) {
		return []Problem{problem("mise.toml", "check:modules must run the three Go module checks serially")}
	}
	return nil
}

func checkOfflineModuleChecks(root string) []Problem {
	var problems []Problem
	for _, where := range []string{"agents/go/mise.toml", "evals/mise.toml", "tools/mise.toml"} {
		dependencies := taskDependencies(filepath.Join(root, filepath.FromSlash(where)), "check")
		if slices.Contains(dependencies, "check:vuln") {
			problems = append(problems, problem(where, "offline module check must not depend on networked check:vuln"))
		}
	}
	return problems
}

func checkReadOnlyInstalls(root string) []Problem {
	var problems []Problem
	updateBearing := regexp.MustCompile(`\b(?:hugo mod get|go mod tidy|go get|gcloud\s+components\s+install)\b`)
	for _, where := range []string{"mise.toml", "agents/go/mise.toml", "evals/mise.toml", "tools/mise.toml"} {
		path := filepath.Join(root, filepath.FromSlash(where))
		for name := range taskNames(path) {
			if name != "install" && !strings.HasPrefix(name, "install:") {
				continue
			}
			for _, command := range taskCommands(path, name) {
				problems = append(problems, checkMiseInstallFlags(where, fmt.Sprintf("install task %q", name), command)...)
				if strings.Contains(command, "command -v gke-gcloud-auth-plugin") {
					problems = append(problems, problem(where,
						"install task %q accepts an ambient GKE auth plugin as installation proof", name))
				}
				if found := updateBearing.FindString(command); found != "" {
					problems = append(problems, problem(where, "install task %q runs update-bearing %q", name, found))
				}
				if strings.Contains(command, "go mod download") && !strings.Contains(command, "GOFLAGS=-mod=readonly") {
					problems = append(problems, problem(where, "install task %q must run go mod download with GOFLAGS=-mod=readonly", name))
				}
			}
		}
	}
	problems = append(problems, checkAutomationMiseInstallFlags(root)...)

	// The documentation build folded into CI; its browser-accessibility job is what still
	// installs the documentation toolchain, so that is the file this contract now reads.
	const docsWorkflow = ".github/workflows/ci.yml"
	docs, err := readFile(filepath.Join(root, filepath.FromSlash(docsWorkflow)))
	if err != nil {
		return append(problems, problem(docsWorkflow, "could not read documentation install contract: %v", err))
	}
	for _, found := range updateBearing.FindAllString(docs, -1) {
		problems = append(problems, problem(docsWorkflow, "documentation install runs update-bearing %q", found))
	}
	for _, line := range splitLines(docs) {
		if strings.Contains(line, "go mod download") && !strings.Contains(line, "GOFLAGS=-mod=readonly") {
			problems = append(problems, problem(docsWorkflow, "documentation install must run go mod download with GOFLAGS=-mod=readonly"))
		}
	}
	return problems
}

func checkNestedMiseTrust(pages pageSet) []Problem {
	const where = "content/1. Setup/1.0. System.md"
	page := pages[where]
	var problems []Problem
	if strings.Contains(page, "mise trust --all") {
		problems = append(problems, problem(where,
			"clean-clone setup must not use mise trust --all because it walks parents and unrelated nested configs"))
	}
	for _, config := range []string{
		"mise.toml",
		"agents/go/mise.toml",
		"agents/data/mise.toml",
		"evals/mise.toml",
		"tools/mise.toml",
	} {
		command := "mise trust " + config
		if !strings.Contains(page, command) {
			problems = append(problems, problem(where,
				"clean-clone setup must trust the reviewed config explicitly with %q", command))
		}
	}
	return problems
}

func checkMiseInstallFlags(where, subject, command string) []Problem {
	fields := strings.Fields(command)
	for index := 0; index+1 < len(fields); index++ {
		if fields[index] != "mise" || fields[index+1] != "install" {
			continue
		}

		arguments := fields[index+2:]
		var problems []Problem
		if !slices.Contains(arguments, "--locked") {
			problems = append(problems, problem(where, "%s must pass --locked to mise install", subject))
		}
		if !slices.Contains(arguments, "-y") && !slices.Contains(arguments, "--yes") {
			problems = append(problems, problem(where, "%s must pass -y or --yes to mise install", subject))
		}
		return problems
	}
	return nil
}

func checkAutomationMiseInstallFlags(root string) []Problem {
	var problems []Problem
	err := filepath.WalkDir(filepath.Join(root, ".github"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}

		where := relative(root, path)
		workflow := strings.HasPrefix(where, ".github/workflows/") && slices.Contains([]string{".yml", ".yaml"}, filepath.Ext(path))
		action := strings.HasPrefix(where, ".github/actions/") && slices.Contains([]string{"action.yml", "action.yaml"}, filepath.Base(path))
		if !workflow && !action {
			return nil
		}

		content, err := readFile(path)
		if err != nil {
			return err
		}
		for _, line := range splitLines(content) {
			problems = append(problems, checkMiseInstallFlags(where, "automation install", line)...)
		}
		return nil
	})
	if err != nil {
		problems = append(problems, problem(".github", "could not inspect automation installs: %v", err))
	}
	return problems
}

func checkHugoExtendedTool(root string) []Problem {
	var problems []Problem
	if toolVersion(root, "hugo-extended") == "" {
		problems = append(problems, problem("mise.toml", "Hugo Extended must be pinned as the hugo-extended tool"))
	}
	if toolVersion(root, "hugo") != "" {
		problems = append(problems, problem("mise.toml", "the standard Hugo tool cannot satisfy the documented Hugo Extended contract"))
	}
	for _, where := range []string{"mise.toml", ".github/workflows/ci.yml"} {
		text, err := readFile(filepath.Join(root, filepath.FromSlash(where)))
		if err != nil {
			problems = append(problems, problem(where, "could not read the Hugo install contract: %v", err))
			continue
		}
		for _, line := range splitLines(text) {
			fields := strings.Fields(line)
			for index := 0; index+2 < len(fields); index++ {
				if fields[index] == "mise" && fields[index+1] == "install" && fields[index+2] == "hugo" {
					problems = append(problems, problem(where, "installs standard Hugo instead of hugo-extended"))
				}
			}
		}
	}
	return problems
}

func checkDocumentedTasks(root string, pages pageSet) []Problem {
	known := map[string]bool{"install:core": true, "watch": true, "scan": true}
	for _, manifest := range []string{"mise.toml", "agents/go/mise.toml", "agents/data/mise.toml", "evals/mise.toml", "tools/mise.toml"} {
		for name := range taskNames(filepath.Join(root, filepath.FromSlash(manifest))) {
			known[name] = true
		}
	}
	ignored := map[string]bool{"doctor:PROFILE": true, "tasks": true}
	pattern := regexp.MustCompile(`\bmise run ([A-Za-z0-9][A-Za-z0-9:_*-]*(?:<[^>]+>)?)`)
	var problems []Problem
	for where, text := range pages {
		for _, match := range pattern.FindAllStringSubmatch(text, -1) {
			name := match[1]
			if !known[name] && !ignored[name] && !strings.ContainsAny(name, "<*") {
				problems = append(problems, problem(where, "documents unknown mise task %q", name))
			}
		}
	}
	return problems
}

func checkTaskExpansions(root string, pages pageSet) []Problem {
	const where = "content/3. Capabilities/3.0. Packaging.md"
	pattern := regexp.MustCompile(`(?m)^\| ` + "`" + `mise run ([^` + "`" + `]+)` + "`" + `\s+\| ` + "`" + `([^` + "`" + `]+)` + "`" + `\s+\|`)
	documented := make(map[string]string)
	for _, match := range pattern.FindAllStringSubmatch(pages[where], -1) {
		documented[match[1]] = match[2]
	}
	manifest := filepath.Join(root, "agents", "go", "mise.toml")
	problems := make([]Problem, 0, 8)
	for _, name := range []string{"run", "workflow", "coordinator", "web", "mcp", "mcp:http", "a2a", "data:reset"} {
		problems = append(problems, compareContract(where, "mise run "+name+" expansion", taskExpansion(manifest, name), documented[name])...)
	}
	return problems
}
