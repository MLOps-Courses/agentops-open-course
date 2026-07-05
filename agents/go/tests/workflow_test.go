package tests

import (
	"context"
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/workflow"
)

func TestWorkflowChainsThreeSteps(t *testing.T) {
	store := openStore(t)

	// Model construction makes no live call, but the Gemini client requires a non-empty
	// key — a dummy is enough, keeping this structural test green in CI without a secret.
	t.Setenv("GOOGLE_API_KEY", "test-key-no-live-calls")

	wf, err := workflow.New(context.Background(), config.Load(), store)
	if err != nil {
		t.Fatalf("workflow.New: %v", err)
	}
	if wf.Name() != "triage_workflow" {
		t.Fatalf("workflow name = %q, want triage_workflow", wf.Name())
	}

	names := make([]string, 0, len(wf.SubAgents()))
	for _, sub := range wf.SubAgents() {
		names = append(names, sub.Name())
	}
	want := []string{"triage", "diagnose", "recommend"}
	if len(names) != len(want) {
		t.Fatalf("sub-agents = %v, want %v", names, want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Fatalf("sub-agent[%d] = %q, want %q", i, names[i], n)
		}
	}
}
