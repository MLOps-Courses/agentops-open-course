package tests

import (
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/actions"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/guardrails"
)

func TestSetServiceStatusAndResolve(t *testing.T) {
	store := openStore(t)

	if svc, _, _ := store.GetService("inventory"); svc.Status != "down" {
		t.Fatalf("precondition: inventory status = %q, want down", svc.Status)
	}
	changed, err := store.SetServiceStatus("inventory", "operational")
	if err != nil || !changed {
		t.Fatalf("SetServiceStatus: changed=%v err=%v", changed, err)
	}
	if svc, _, _ := store.GetService("inventory"); svc.Status != "operational" {
		t.Fatalf("inventory status = %q, want operational", svc.Status)
	}

	resolved, err := store.ResolveIncident("INC-002")
	if err != nil || !resolved {
		t.Fatalf("ResolveIncident(INC-002): resolved=%v err=%v", resolved, err)
	}
	if inc, _, _ := store.GetIncident("INC-002"); inc.Status != "resolved" || inc.ResolvedAt == "" {
		t.Fatalf("INC-002 = %+v, want resolved with a timestamp", inc)
	}

	// Resolving an already-resolved incident is a no-op.
	if again, _ := store.ResolveIncident("INC-002"); again {
		t.Fatal("expected resolving an already-resolved incident to change nothing")
	}
}

func TestActionToolsRequireConfirmation(t *testing.T) {
	store := openStore(t)

	all, err := actions.All(store)
	if err != nil {
		t.Fatalf("actions.All: %v", err)
	}
	got := map[string]bool{}
	for _, tl := range all {
		got[tl.Name()] = true
	}
	for _, want := range []string{"restart_service", "resolve_incident"} {
		if !got[want] {
			t.Errorf("missing guarded action %q (have %v)", want, got)
		}
	}
}

func TestGuardrailBlocksBadInput(t *testing.T) {
	store := openStore(t)
	all, err := actions.All(store)
	if err != nil {
		t.Fatalf("actions.All: %v", err)
	}
	byName := map[string]int{}
	for i, tl := range all {
		byName[tl.Name()] = i
	}
	resolve := all[byName["resolve_incident"]]

	blocked, err := guardrails.ValidateActions(nil, resolve, map[string]any{"incident_id": "oops"})
	if err != nil {
		t.Fatalf("ValidateActions: %v", err)
	}
	if blocked == nil || blocked["error"] == nil {
		t.Fatalf("expected a block for a malformed id, got %v", blocked)
	}

	ok, err := guardrails.ValidateActions(nil, resolve, map[string]any{"incident_id": "INC-002"})
	if err != nil {
		t.Fatalf("ValidateActions: %v", err)
	}
	if ok != nil {
		t.Fatalf("expected valid input to pass, got %v", ok)
	}
}
