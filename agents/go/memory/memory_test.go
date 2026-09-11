package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/data"
)

// This file covers the package's own wiring: what [New] refuses, and what a
// broken store looks like to everything downstream.

func TestNewRefusesAnIncompleteWiring(t *testing.T) {
	t.Parallel()

	cases := []struct {
		mutate func(*Config)
		name   string
		want   string
	}{
		{func(c *Config) { c.Store = nil }, "no store", "Store is required"},
		{func(c *Config) { c.Guard = nil }, "no guard", "Guard is required"},
		{func(c *Config) { c.Redact = nil }, "no redactor", "Redact is required"},
		{func(c *Config) { c.StateDir = "" }, "no state directory", "StateDir is required"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			cfg := Config{
				Store:    stubStore{},
				Guard:    func(ctx context.Context, _ string, call func(context.Context) error) error { return call(ctx) },
				Redact:   func(text string) string { return text },
				StateDir: t.TempDir(),
			}
			testCase.mutate(&cfg)

			// Every hole is a capability the agent's own instruction claims it has,
			// and silence is the wrong failure mode for a security control: a nil
			// redactor would persist a credential, a nil guard would leave a read
			// with no deadline. The composition has to state its choice.
			_, err := New(cfg)
			if !errors.Is(err, ErrIncompleteConfig) {
				t.Fatalf("New() error = %v, want %v", err, ErrIncompleteConfig)
			}
			contains(t, err.Error(), testCase.want, "the refusal")
		})
	}
}

func TestNewReportsEveryHoleAtOnce(t *testing.T) {
	t.Parallel()

	// A wiring with four holes reports four lines: this is assembled once at
	// startup, and an operator fixing it wants the whole list in one round trip.
	_, err := New(Config{})
	if !errors.Is(err, ErrIncompleteConfig) {
		t.Fatalf("New() error = %v, want %v", err, ErrIncompleteConfig)
	}
	for _, want := range []string{"Store is required", "Guard is required", "Redact is required", "StateDir is required"} {
		contains(t, err.Error(), want, "the refusal")
	}
}

func TestAnUnusableNoteStoreSurfacesAsADataAccessFailure(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	// A directory where the database file belongs. SQLite cannot open it, which
	// is the cheapest reproduction of "the state volume is broken".
	if err := os.MkdirAll(filepath.Join(fixture.stateDir, notesDatabaseName), 0o750); err != nil {
		t.Fatalf("stage the unusable store: %v", err)
	}

	_, err := run(t, fixture.memory.RecallIncidentContext(), engineerContext(t), map[string]any{})
	if err == nil {
		t.Fatal("RecallIncidentContext() error = nil, want the store failure")
	}
	// Every SQLite store in the agent surfaces one contract. The composition's
	// error classifier already treats it as safe to explain to the model, so a
	// sentinel of this package's own would silently make memory failures opaque.
	if !errors.Is(err, data.ErrDataAccess) {
		t.Errorf("error = %v, want it to wrap %v", err, data.ErrDataAccess)
	}
	contains(t, err.Error(), "Long-term memory operation failed", "the failure")
	// The file name, not the whole path: the message reaches a model, and an
	// absolute path is deployment layout it has no use for.
	if strings.Contains(err.Error(), fixture.dataDir) {
		t.Errorf("the failure %q leaks the dataset path", err)
	}
}
