package tests

import (
	"context"
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/skills"
)

func TestSkillToolsetLoadsSkills(t *testing.T) {
	toolset, err := skills.Toolset(context.Background(), config.Load().DataDir)
	if err != nil {
		t.Fatalf("skills.Toolset: %v", err)
	}
	tools, err := toolset.Tools(nil)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("expected the skill toolset to expose at least one tool")
	}
}
