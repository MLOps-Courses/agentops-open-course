// Package tests holds integration-style tests for the Go agent that do not need a provider key.
package tests

import (
	"os"
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/data"
)

// openStore copies the whole bundled dataset (db + runbooks) into a temp dir so writing
// tests (audit log) never mutate the committed files, then opens a Store against the copy.
func openStore(t *testing.T) *data.Store {
	t.Helper()
	dst := t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(config.Load().DataDir)); err != nil {
		t.Fatalf("copying dataset: %v", err)
	}
	store, err := data.Open(dst)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestListIncidents(t *testing.T) {
	store := openStore(t)

	all, err := store.ListIncidents("", "")
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	if len(all) < 6 {
		t.Fatalf("expected >= 6 seeded incidents, got %d", len(all))
	}

	open, err := store.ListIncidents("open", "")
	if err != nil {
		t.Fatalf("ListIncidents(open): %v", err)
	}
	if len(open) == 0 {
		t.Fatal("expected at least one open incident")
	}
	for _, inc := range open {
		if inc.Status != "open" {
			t.Fatalf("status filter leaked: %+v", inc)
		}
	}
}

func TestGetIncident(t *testing.T) {
	store := openStore(t)

	inc, ok, err := store.GetIncident("INC-001")
	if err != nil || !ok {
		t.Fatalf("GetIncident(INC-001): ok=%v err=%v", ok, err)
	}
	if inc.Runbook != "high-latency" {
		t.Fatalf("runbook = %q, want high-latency", inc.Runbook)
	}

	if _, ok, _ := store.GetIncident("INC-999"); ok {
		t.Fatal("expected INC-999 to be unknown")
	}
}

func TestGetService(t *testing.T) {
	store := openStore(t)

	svc, ok, err := store.GetService("checkout")
	if err != nil || !ok {
		t.Fatalf("GetService(checkout): ok=%v err=%v", ok, err)
	}
	if svc.Status != "degraded" {
		t.Fatalf("checkout status = %q, want degraded", svc.Status)
	}

	if _, ok, _ := store.GetService("nope"); ok {
		t.Fatal("expected unknown service to return ok=false")
	}
}

func TestAppendAudit(t *testing.T) {
	store := openStore(t)

	entry, err := store.AppendAudit("test", "noop", "checkout", "unit test")
	if err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	if entry.Actor != "test" || entry.ID == 0 {
		t.Fatalf("unexpected audit entry: %+v", entry)
	}
}
