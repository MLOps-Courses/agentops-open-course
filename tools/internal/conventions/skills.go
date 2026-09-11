package conventions

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// CheckSkills validates every installable skill's portable metadata surface.
func CheckSkills(root string) []Problem {
	files, _ := filepath.Glob(filepath.Join(root, "skills", "*", "SKILL.md"))
	slices.Sort(files)
	if len(files) == 0 {
		return []Problem{problem("skills", "no SKILL.md found under skills/")}
	}
	var problems []Problem
	for _, path := range files {
		where := relative(root, path)
		content, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, problem(where, "could not read skill: %v", err))
			continue
		}
		text := string(content)
		metadata, message := parseFrontMatter(text)
		if metadata == nil {
			if message == "" {
				message = "must start with YAML front matter (---)"
			}
			problems = append(problems, problem(where, "%s", message))
			continue
		}
		declared, _ := metadata["name"].(string)
		directory := filepath.Base(filepath.Dir(path))
		if declared == "" {
			problems = append(problems, problem(where, "front matter is missing a name"))
		} else if declared != directory {
			problems = append(problems, problem(where, "name %q must match its directory %q", declared, directory))
		}
		description, _ := metadata["description"].(string)
		if description == "" {
			problems = append(problems, problem(where, "front matter is missing a description"))
		}
		h1 := false
		for _, line := range splitLines(text) {
			h1 = h1 || strings.HasPrefix(line, "# ")
		}
		if !h1 {
			problems = append(problems, problem(where, "expected an H1 title"))
		}
		if machinePathPattern.MatchString(text) {
			problems = append(problems, problem(where, "found a machine-specific path (skills must be portable)"))
		}
	}
	return problems
}
