package conventions

// GitHub Actions contracts. Every check here reads workflow and composite-action
// YAML: checkout order, Pages deployment authority, and untrusted interpolation.

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// workflowFiles lists every GitHub Actions workflow the repository ships, sorted.
//
// GitHub honors both `.yml` and `.yaml`, so a check that globs only one of them is a
// gate a single file rename walks around: the Pages-deployment and artifact-retention
// contracts both used to glob `*.yml` alone and would have reported green on a `.yaml`
// workflow that broke them. Both go through here now. The other two readers,
// checkLocalActionCheckouts and checkWorkflowShellExpressions, walk the directory with
// os.ReadDir and already filter on both extensions themselves; they stay that way because
// they report an unreadable directory as a problem, which this glob deliberately cannot.
// So the rule is the invariant, not the function: a workflow reader honors both extensions.
func workflowFiles(root string) []string {
	var paths []string
	for _, extension := range []string{"*.yml", "*.yaml"} {
		matches, _ := filepath.Glob(filepath.Join(root, ".github", "workflows", extension))
		paths = append(paths, matches...)
	}
	slices.Sort(paths)
	return paths
}

func checkLocalActionCheckouts(root string) []Problem {
	directory := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return []Problem{problem(".github/workflows", "could not read workflows: %v", err)}
	}
	var problems []Problem
	for _, entry := range entries {
		if entry.IsDir() || (filepath.Ext(entry.Name()) != ".yml" && filepath.Ext(entry.Name()) != ".yaml") {
			continue
		}
		where := filepath.ToSlash(filepath.Join(".github", "workflows", entry.Name()))
		content, readErr := os.ReadFile(filepath.Join(directory, entry.Name()))
		if readErr != nil {
			problems = append(problems, problem(where, "could not read workflow: %v", readErr))
			continue
		}
		var workflow struct {
			Jobs map[string]struct {
				Steps []struct {
					Uses string `yaml:"uses"`
				} `yaml:"steps"`
			} `yaml:"jobs"`
		}
		if parseErr := yaml.Unmarshal(content, &workflow); parseErr != nil {
			problems = append(problems, problem(where, "could not parse workflow: %v", parseErr))
			continue
		}
		for name, job := range workflow.Jobs {
			checkedOut := false
			for _, step := range job.Steps {
				if strings.HasPrefix(step.Uses, "actions/checkout@") {
					checkedOut = true
				}
				if strings.HasPrefix(step.Uses, "./") && !checkedOut {
					problems = append(problems, problem(where,
						"job %q uses repository-local action %q before actions/checkout", name, step.Uses))
					break
				}
			}
		}
	}
	return problems
}

// checkPagesDeployment pins Pages deployment authority to exactly one job.
//
// This check used to refuse Pages deployment anywhere, from the period when the Go
// rewrite lived in its own checkout and deliberately published nothing. The course now
// serves agentops-open-course.fmind.dev, so the risk inverted: the danger is no longer a
// stray deploy but a second one racing the real one, or the real one disappearing in a
// refactor and the site silently freezing on its last good build. Exactly one job may
// hold the authority, and it must be the documentation deploy.
const pagesDeployJob = "deploy"

func checkPagesDeployment(root string) []Problem {
	paths := workflowFiles(root)
	if len(paths) == 0 {
		return []Problem{problem(".github/workflows", "could not read the workflow directory")}
	}
	var problems []Problem
	var deployers []string
	for _, path := range paths {
		where := ".github/workflows/" + filepath.Base(path)
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			problems = append(problems, problem(where, "could not read workflow: %v", readErr))
			continue
		}
		var workflow struct {
			Jobs map[string]struct {
				Environment any               `yaml:"environment"`
				Permissions map[string]string `yaml:"permissions"`
				Steps       []struct {
					Uses string `yaml:"uses"`
				} `yaml:"steps"`
			} `yaml:"jobs"`
		}
		if unmarshalErr := yaml.Unmarshal(content, &workflow); unmarshalErr != nil {
			problems = append(problems, problem(where, "could not parse workflow: %v", unmarshalErr))
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(workflow.Jobs)) {
			job := workflow.Jobs[name]
			// Deployment authority, not Pages involvement. `configure-pages` and
			// `upload-pages-artifact` only prepare and hand over an artifact and are
			// harmless in a build job; what actually publishes is `deploy-pages`, and
			// what actually grants the right to is the github-pages environment. Keying
			// on the build-side actions too would make a conventional two-job Pages
			// workflow look like two deployers.
			pagesAuthority := pagesEnvironment(job.Environment) || job.Permissions["pages"] != ""
			for _, step := range job.Steps {
				pagesAuthority = pagesAuthority || strings.HasPrefix(step.Uses, "actions/deploy-pages@")
			}
			if pagesAuthority {
				deployers = append(deployers, where+":"+name)
			}
		}
	}
	switch {
	case len(deployers) == 0:
		problems = append(problems, problem(".github/workflows",
			"no job holds Pages deployment authority; the course site would never reach %s", RepositorySlug))
	case len(deployers) > 1:
		problems = append(problems, problem(".github/workflows",
			"Pages deployment authority is split across %v; exactly one job may deploy the site", deployers))
	case !strings.HasSuffix(deployers[0], ":"+pagesDeployJob):
		problems = append(problems, problem(deployers[0],
			"the Pages deployment job must be named %q so the branch ruleset can require it", pagesDeployJob))
	}
	return problems
}

func pagesEnvironment(value any) bool {
	switch typed := value.(type) {
	case string:
		return typed == "github-pages"
	case map[string]any:
		name, _ := typed["name"].(string)
		return name == "github-pages"
	default:
		return false
	}
}

func checkWorkflowShellExpressions(root string) []Problem {
	directory := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return []Problem{problem(".github/workflows", "could not read workflows: %v", err)}
	}
	var problems []Problem
	directMatrix := regexp.MustCompile(`\$\{\{\s*matrix\.`)
	for _, entry := range entries {
		if entry.IsDir() || (filepath.Ext(entry.Name()) != ".yml" && filepath.Ext(entry.Name()) != ".yaml") {
			continue
		}
		where := filepath.ToSlash(filepath.Join(".github", "workflows", entry.Name()))
		content, readErr := os.ReadFile(filepath.Join(directory, entry.Name()))
		if readErr != nil {
			problems = append(problems, problem(where, "could not read workflow: %v", readErr))
			continue
		}
		var workflow struct {
			Jobs map[string]struct {
				Steps []struct {
					Run string `yaml:"run"`
				} `yaml:"steps"`
			} `yaml:"jobs"`
		}
		if parseErr := yaml.Unmarshal(content, &workflow); parseErr != nil {
			problems = append(problems, problem(where, "could not parse workflow: %v", parseErr))
			continue
		}
		for name, job := range workflow.Jobs {
			for _, step := range job.Steps {
				if directMatrix.MatchString(step.Run) {
					problems = append(problems, problem(where,
						"job %q interpolates matrix context directly in shell; pass it through env", name))
				}
			}
		}
	}

	actionsRoot := filepath.Join(root, ".github", "actions")
	directInput := regexp.MustCompile(`\$\{\{\s*inputs\.`)
	actions, openErr := os.OpenRoot(actionsRoot)
	if os.IsNotExist(openErr) {
		return problems
	}
	if openErr != nil {
		return append(problems, problem(".github/actions", "could not read actions: %v", openErr))
	}
	walkErr := fs.WalkDir(actions.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || (entry.Name() != "action.yml" && entry.Name() != "action.yaml") {
			return nil
		}
		where := filepath.ToSlash(filepath.Join(".github", "actions", path))
		content, readErr := actions.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		var action struct {
			Runs struct {
				Steps []struct {
					Run string `yaml:"run"`
				} `yaml:"steps"`
			} `yaml:"runs"`
		}
		if parseErr := yaml.Unmarshal(content, &action); parseErr != nil {
			problems = append(problems, problem(where, "could not parse action: %v", parseErr))
			return nil
		}
		for _, step := range action.Runs.Steps {
			if directInput.MatchString(step.Run) {
				problems = append(problems, problem(where,
					"composite action interpolates an action input directly in shell; pass it through env"))
			}
		}
		return nil
	})
	if walkErr != nil {
		problems = append(problems, problem(".github/actions", "could not read actions: %v", walkErr))
	}
	if closeErr := actions.Close(); closeErr != nil {
		problems = append(problems, problem(".github/actions", "could not close actions root: %v", closeErr))
	}
	return problems
}
