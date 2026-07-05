package tests

import (
	"context"
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/delegation"
)

func TestCoordinatorDelegatesToDiagnosis(t *testing.T) {
	store := openStore(t)

	// Building the Gemini model needs a non-empty key but makes no live call, so a dummy
	// is enough — this keeps the structural test runnable in CI without a provider secret.
	t.Setenv("GOOGLE_API_KEY", "test-key-no-live-calls")

	coordinator, err := delegation.New(context.Background(), config.Load(), store)
	if err != nil {
		t.Fatalf("delegation.New: %v", err)
	}
	if coordinator.Name() != "coordinator_agent" {
		t.Fatalf("coordinator name = %q, want coordinator_agent", coordinator.Name())
	}

	subs := coordinator.SubAgents()
	if len(subs) != 1 || subs[0].Name() != "diagnosis_agent" {
		t.Fatalf("sub-agents = %v, want [diagnosis_agent]", subs)
	}
}
