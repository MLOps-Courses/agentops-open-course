package tests

import (
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/tools"
)

func TestAllToolsRegistered(t *testing.T) {
	store := openStore(t)

	all, err := tools.All(store)
	if err != nil {
		t.Fatalf("tools.All: %v", err)
	}

	got := map[string]bool{}
	for _, tl := range all {
		got[tl.Name()] = true
	}
	for _, want := range []string{"list_incidents", "get_incident", "get_service_status"} {
		if !got[want] {
			t.Errorf("missing tool %q (have %v)", want, got)
		}
	}
}
