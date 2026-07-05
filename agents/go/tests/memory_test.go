package tests

import (
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/memory"
)

func TestRunbookHelpers(t *testing.T) {
	store := openStore(t)

	slugs, err := store.ListRunbookSlugs()
	if err != nil {
		t.Fatalf("ListRunbookSlugs: %v", err)
	}
	if len(slugs) < 5 {
		t.Fatalf("expected >= 5 runbooks, got %d: %v", len(slugs), slugs)
	}

	content, ok, err := store.ReadRunbook("high-latency")
	if err != nil || !ok {
		t.Fatalf("ReadRunbook(high-latency): ok=%v err=%v", ok, err)
	}
	if content == "" {
		t.Fatal("expected non-empty runbook content")
	}

	if _, ok, _ := store.ReadRunbook("nope"); ok {
		t.Fatal("expected unknown runbook to return ok=false")
	}
}

func TestKnowledgeToolsRegistered(t *testing.T) {
	store := openStore(t)

	all, err := memory.KnowledgeTools(store)
	if err != nil {
		t.Fatalf("KnowledgeTools: %v", err)
	}
	got := map[string]bool{}
	for _, tl := range all {
		got[tl.Name()] = true
	}
	for _, want := range []string{"get_runbook", "search_runbooks"} {
		if !got[want] {
			t.Errorf("missing tool %q (have %v)", want, got)
		}
	}
}
