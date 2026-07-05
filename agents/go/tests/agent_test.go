// Package tests holds integration-style tests for the Go agent that do not need a provider key.
package tests

import (
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/agent"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/config"
)

func TestAgentIdentity(t *testing.T) {
	if agent.Name == "" {
		t.Fatal("agent.Name must be set")
	}
	if agent.Instruction == "" {
		t.Fatal("agent.Instruction must be set")
	}
}

func TestConfigDefaultsModel(t *testing.T) {
	t.Setenv("AGENT_MODEL", "")
	if got := config.Load().Model; got != config.DefaultModel {
		t.Fatalf("Model = %q, want %q", got, config.DefaultModel)
	}
}
