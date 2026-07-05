// Package skills loads Agent Skills — progressive-disclosure instructions for the Ops Copilot
// (Chapter 3.2).
//
// A skill is a folder with a SKILL.md (name + description front-matter, then instructions, plus
// optional assets/references). The model first sees only each skill's name and description and
// loads the full body on demand — progressive disclosure that keeps the context small. Skills
// live in agents/data/skills so both tracks share them (the open Agent Skills standard).
package skills

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
)

// Toolset builds a skill toolset from every skill under <dataDir>/skills.
func Toolset(ctx context.Context, dataDir string) (tool.Toolset, error) {
	source := skill.NewFileSystemSource(os.DirFS(filepath.Join(dataDir, "skills")))
	toolset, err := skilltoolset.New(ctx, skilltoolset.Config{Source: source})
	if err != nil {
		return nil, fmt.Errorf("building skill toolset: %w", err)
	}
	return toolset, nil
}
