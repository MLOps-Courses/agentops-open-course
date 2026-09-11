package memory

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/policy"
)

// This file is the Go port of tests/test_longterm.py: the durable, per-user
// incident notes that let one conversation pick up where the last left off.

func TestNotesPersistAcrossSimulatedSessions(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	yesterday := toolContext(t, testApp, testUser, "session-mon")
	saved := saveNote(t, fixture, yesterday, inventoryIncident,
		"Restarted "+inventoryService+"; crash-loop persists.")
	if saved.Saved == nil {
		t.Fatalf("save returned %+v, want a saved note", saved)
	}

	// A brand-new conversation. The whole point of long-term memory is that the
	// session id does not appear anywhere in the scoping.
	today := toolContext(t, testApp, testUser, "session-tue")
	recalled := recall(t, fixture, today, inventoryIncident)

	if *recalled.Count != 1 {
		t.Fatalf("count = %d, want 1", *recalled.Count)
	}
	contains(t, recalled.Notes[0].Note, "crash-loop persists", "recalled note")
	if recalled.Notes[0].IncidentID != inventoryIncident {
		t.Errorf("incident = %q, want %q", recalled.Notes[0].IncidentID, inventoryIncident)
	}
}

func TestRecallWithoutAFilterReturnsNewestFirst(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx := engineerContext(t)
	saveNote(t, fixture, ctx, checkoutIncident, "checked latency graphs")
	saveNote(t, fixture, ctx, inventoryIncident, "escalated to fulfillment")

	recalled := recall(t, fixture, ctx, "")
	if *recalled.Count != 2 {
		t.Fatalf("count = %d, want 2", *recalled.Count)
	}
	// Newest first, by insertion order rather than by timestamp: both notes land
	// in the same second, and a second-resolution stamp cannot order them.
	if recalled.Notes[0].IncidentID != inventoryIncident {
		t.Errorf("newest note is for %q, want %q", recalled.Notes[0].IncidentID, inventoryIncident)
	}
}

func TestMemoryIsIsolatedPerUser(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	saveNote(t, fixture, toolContext(t, testApp, "alice", "s1"), inventoryIncident, "private note from alice")

	recalled := recall(t, fixture, toolContext(t, testApp, "bob", "s2"), inventoryIncident)
	if *recalled.Count != 0 {
		t.Errorf("bob recalled %d of alice's notes, want 0", *recalled.Count)
	}
}

func TestMemoryIsIsolatedPerApplication(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	saveNote(t, fixture, toolContext(t, "one-app", testUser, "s1"), inventoryIncident, "note from the first app")

	// The Python store scoped on the user alone because it served exactly one
	// application. ADK's memory.Service carries an AppName on every search, so
	// honoring it here is what keeps one state directory from leaking notes
	// between two deployments that share it.
	recalled := recall(t, fixture, toolContext(t, "other-app", testUser, "s2"), inventoryIncident)
	if *recalled.Count != 0 {
		t.Errorf("the second application recalled %d notes, want 0", *recalled.Count)
	}
}

func TestNotesAreRedactedBeforeBeingPersisted(t *testing.T) {
	t.Parallel()

	// The real redactor, not a stand-in: this test is the proof that memory is a
	// persistence boundary like the audit trail, so the masks have to be the ones
	// that actually reach disk.
	governance, err := policy.New(policy.Config{})
	if err != nil {
		t.Fatalf("policy.New() error = %v, want nil", err)
	}
	fixture := newFixture(t, func(o *options) {
		o.redact = governance.PersistedRedactor(t.Context())
	})
	ctx := engineerContext(t)
	saveNote(t, fixture, ctx, inventoryIncident,
		"paged jane.doe@acme.com with api_key=super-secret-api-key-123456")

	recalled := recall(t, fixture, ctx, inventoryIncident)
	if *recalled.Count != 1 {
		t.Fatalf("count = %d, want 1", *recalled.Count)
	}
	note := recalled.Notes[0].Note
	for _, secret := range []string{"jane.doe@acme.com", "super-secret-api-key-123456"} {
		if strings.Contains(note, secret) {
			t.Errorf("recalled note %q still contains %q", note, secret)
		}
	}
	contains(t, note, "<EMAIL_ADDRESS>", "recalled note")
	contains(t, note, "api_key="+policy.SecretMask, "recalled note")

	// And the raw text really did go through the seam before the write, rather
	// than the store having been cleaned afterwards.
	if len(fixture.redacted.all()) == 0 {
		t.Fatal("the redactor was never called")
	}
	contains(t, fixture.redacted.all()[0], "jane.doe@acme.com", "text handed to the redactor")

	// The bytes on disk, read outside the code under test: a redaction that only
	// held on the way out would still have leaked the original.
	stored := storedNotes(t, fixture)
	if len(stored) != 1 {
		t.Fatalf("the store holds %d notes, want 1", len(stored))
	}
	if strings.Contains(stored[0], "jane.doe@acme.com") {
		t.Errorf("the persisted row %q still contains the address", stored[0])
	}
}

func TestTheKillSwitchRefusesANoteBeforeAnyStateIsWritten(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(o *options) { o.writesDisabled = true })
	result := saveNote(t, fixture, engineerContext(t), inventoryIncident, "Restarted "+inventoryService+".")

	if result.Saved != nil {
		t.Fatalf("result = %+v, want a refusal", result)
	}
	contains(t, result.Error, "AGENT_WRITES_DISABLED", "refusal")
	// Nothing was created, not even the database: the kill-switch is consulted
	// before anything touches the filesystem, so a frozen agent leaves no trace.
	if _, err := os.Stat(filepath.Join(fixture.stateDir, notesDatabaseName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat(%s) error = %v, want it to be absent", notesDatabaseName, err)
	}
}

func TestInvalidNoteInputsAreRejected(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx := engineerContext(t)

	cases := []struct {
		name       string
		incidentID string
		note       string
		want       string
	}{
		{"malformed id", "ticket-9", "note", "Invalid incident id"},
		{"unknown incident", "INC-99999", "note", "orphaned memory"},
		{"blank note", inventoryIncident, "   ", "empty note"},
		{"oversized note", inventoryIncident, strings.Repeat("x", maxNoteLength+1), "too long"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			result := saveNote(t, fixture, ctx, testCase.incidentID, testCase.note)
			if result.Saved != nil {
				t.Fatalf("result = %+v, want a refusal", result)
			}
			contains(t, result.Error, testCase.want, "refusal")
		})
	}

	t.Run("recall of a malformed id", func(t *testing.T) {
		t.Parallel()

		recalled := recall(t, fixture, ctx, "not-an-id")
		if recalled.Count != nil {
			t.Fatalf("result = %+v, want a refusal", recalled)
		}
		contains(t, recalled.Error, "Invalid incident id", "refusal")
	})
}

func TestANoteExactlyAtTheLengthBoundIsAccepted(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	// The bound is "keep it under 2000", enforced as a rune count, so a note of
	// exactly that many characters is the last accepted one — and an accented
	// note of the same length must not be judged twice as long.
	result := saveNote(t, fixture, engineerContext(t), inventoryIncident, strings.Repeat("é", maxNoteLength))
	if result.Saved == nil {
		t.Fatalf("result = %+v, want the note to be saved", result)
	}
}

func TestDirectCallsUseAStableAnonymousIdentity(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	// No ADK context at all — a script or an operator command reaching the
	// handler directly, which is what the Python track did by passing
	// tool_context=None. It must still be able to recall what it saved, which
	// needs a stable identity rather than a generated one. The handlers are
	// called rather than the tools because ADK's own wrapper dereferences the
	// context before the handler ever runs.
	saved, err := fixture.memory.runSaveIncidentNote(nil, SaveIncidentNoteArgs{
		IncidentID: checkoutIncident,
		Note:       "saved without a session",
	})
	if err != nil {
		t.Fatalf("runSaveIncidentNote() error = %v, want nil", err)
	}
	if saved.Saved == nil {
		t.Fatalf("result = %+v, want the note to be saved", saved)
	}

	recalled, err := fixture.memory.runRecallIncidentContext(nil,
		RecallIncidentContextArgs{IncidentID: checkoutIncident})
	if err != nil {
		t.Fatalf("runRecallIncidentContext() error = %v, want nil", err)
	}
	if *recalled.Count != 1 {
		t.Errorf("count = %d, want 1", *recalled.Count)
	}
	if scopeOf(nil) != (scope{appName: anonymousScope, userID: anonymousScope}) {
		t.Errorf("scopeOf(nil) = %+v, want the anonymous scope on both axes", scopeOf(nil))
	}
}

func TestMemoryLivesInTheDisposableStateDirectory(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	path, err := fixture.memory.Notes().Path()
	if err != nil {
		t.Fatalf("Path() error = %v, want nil", err)
	}
	// Disposable state, never the committed dataset: `mise run data:reset`
	// deletes this directory, and everything in it has to be rebuildable.
	if want := filepath.Join(fixture.stateDir, notesDatabaseName); path != want {
		t.Errorf("Path() = %q, want %q", path, want)
	}
	if !strings.HasPrefix(path, fixture.stateDir) {
		t.Errorf("Path() = %q, want it under the state directory %q", path, fixture.stateDir)
	}
}

func TestForgetUserMemoryErasesOnlyThatUser(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	alice := toolContext(t, testApp, "alice", "s1")
	bob := toolContext(t, testApp, "bob", "s2")
	saveNote(t, fixture, alice, inventoryIncident, "alice private note")
	saveNote(t, fixture, bob, checkoutIncident, "bob private note")

	forgotten, err := fixture.memory.Notes().ForgetUserMemory(t.Context(), "alice")
	if err != nil {
		t.Fatalf("ForgetUserMemory() error = %v, want nil", err)
	}
	if want := (ForgottenMemory{UserID: "alice", Count: 1}); forgotten != want {
		t.Errorf("ForgetUserMemory() = %+v, want %+v", forgotten, want)
	}
	if count := *recall(t, fixture, alice, "").Count; count != 0 {
		t.Errorf("alice still recalls %d notes, want 0", count)
	}
	if count := *recall(t, fixture, bob, "").Count; count != 1 {
		t.Errorf("bob recalls %d notes, want 1 — erasure is per subject, not global", count)
	}
}

func TestForgetUserMemoryRejectsAnEmptyUser(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	_, err := fixture.memory.Notes().ForgetUserMemory(t.Context(), "   ")
	if !errors.Is(err, ErrEmptyUserID) {
		t.Errorf("ForgetUserMemory(\"   \") error = %v, want %v", err, ErrEmptyUserID)
	}
}

func TestErasureIsNotAnAgentTool(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	// Erasure is an operator action under a different legal basis from the audit
	// trail. The model may save and recall; it must never be handed the ability
	// to erase, which is expressed as the absence of a tool rather than a rule
	// in a prompt.
	for _, target := range fixture.memory.MemoryTools() {
		if strings.Contains(target.Name(), "forget") {
			t.Errorf("MemoryTools() exposes %q", target.Name())
		}
	}
	want := []string{RecallIncidentContextToolName, SaveIncidentNoteToolName}
	got := make([]string, 0, len(fixture.memory.MemoryTools()))
	for _, target := range fixture.memory.MemoryTools() {
		got = append(got, target.Name())
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MemoryTools() = %v, want %v", got, want)
	}
}

func TestRecallIsGuardedAndSaveIsNot(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx := engineerContext(t)
	saveNote(t, fixture, ctx, inventoryIncident, "a note")
	recall(t, fixture, ctx, "")

	// Recall is idempotent, so it carries the deadline, the bounded retries and
	// the circuit breaker. The write deliberately does not: an automatic retry
	// could re-deliver it across an unknown commit boundary and leave two copies
	// of one observation in memory.
	want := []string{RecallIncidentContextToolName}
	if !reflect.DeepEqual(fixture.guard.names.all(), want) {
		t.Errorf("guarded tools = %v, want %v", fixture.guard.names.all(), want)
	}
}

func TestRecallIsBoundedByTheRecallLimit(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	ctx := engineerContext(t)
	for index := range recallLimit + 5 {
		saveNote(t, fixture, ctx, inventoryIncident, "observation "+strings.Repeat("i", index+1))
	}
	recalled := recall(t, fixture, ctx, "")

	// The model gets recent context, not a complete history it would then have to
	// summarize inside its own window.
	if *recalled.Count != recallLimit {
		t.Errorf("count = %d, want the recall limit %d", *recalled.Count, recallLimit)
	}
}

func TestANoteRequiresAnExistingIncident(t *testing.T) {
	t.Parallel()

	failure := errAssertion
	fixture := newFixture(t, func(o *options) {
		o.store = func(real Store) Store {
			return stubStore{Store: real, getIncident: func() (*domain.Incident, error) { return nil, failure }}
		}
	})
	// A dataset that cannot answer "does this incident exist" is a failure, not a
	// refusal: saving anyway would create the orphaned memory the check exists to
	// prevent, and refusing would tell the model the incident is unknown.
	if _, err := run(t, fixture.memory.SaveIncidentNote(), engineerContext(t),
		map[string]any{"incident_id": inventoryIncident, "note": "a note"}); err == nil {
		t.Fatal("SaveIncidentNote() error = nil, want the store failure")
	}
}

// saveNote runs save_incident_note and decodes its wire result.
func saveNote(t *testing.T, fixture *fixture, ctx agent.Context, incidentID, note string) SaveIncidentNoteResult {
	t.Helper()

	return mustRun[SaveIncidentNoteResult](t, fixture.memory.SaveIncidentNote(), ctx,
		map[string]any{"incident_id": incidentID, "note": note})
}

// recall runs recall_incident_context and decodes its wire result. An empty
// incident id recalls across every incident.
func recall(t *testing.T, fixture *fixture, ctx agent.Context, incidentID string) RecallIncidentContextResult {
	t.Helper()

	args := map[string]any{}
	if incidentID != "" {
		args["incident_id"] = incidentID
	}
	return mustRun[RecallIncidentContextResult](t, fixture.memory.RecallIncidentContext(), ctx, args)
}
