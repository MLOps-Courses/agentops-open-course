package memory

import (
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/data"
)

// openStore opens the committed dataset read-only (search never writes) for ranking tests.
func openStore(t *testing.T) *data.Store {
	t.Helper()
	store, err := data.Open(config.Load().DataDir)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSearchRunbooksRanksServiceDownFirst(t *testing.T) {
	store := openStore(t)

	out, err := searchRunbooks(store, "service is completely down and returning 503", 3)
	if err != nil {
		t.Fatalf("searchRunbooks: %v", err)
	}
	if out.Count == 0 || out.Runbooks[0].Slug != "service-down" {
		t.Fatalf("want service-down ranked first, got %+v", out)
	}
}

func TestSearchRunbooksLimitAndEmpty(t *testing.T) {
	store := openStore(t)

	out, err := searchRunbooks(store, "latency errors disk deploy service", 2)
	if err != nil {
		t.Fatalf("searchRunbooks: %v", err)
	}
	if out.Count > 2 {
		t.Fatalf("limit not respected: %d", out.Count)
	}

	none, err := searchRunbooks(store, "zzzznomatchzzzz", 3)
	if err != nil {
		t.Fatalf("searchRunbooks: %v", err)
	}
	if none.Count != 0 {
		t.Fatalf("expected no matches, got %d", none.Count)
	}
}
