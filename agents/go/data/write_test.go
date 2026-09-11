package data

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// noopAuditRequest is the general append the guarded actions do not cover: any
// action name is allowed on this path.
func noopAuditRequest(invocationID string) AuditRequest {
	return AuditRequest{
		AuditIdentity:  testIdentity(invocationID),
		ContextSummary: "test context",
		Action:         "noop",
		Target:         string(checkoutService),
		Detail:         "test",
	}
}

func TestEveryWriterMigratesRuntimeStateFirst(t *testing.T) {
	t.Parallel()
	// Python counted calls to prepare_runtime_database through a monkeypatch.
	// Asserting the *effect* is stronger and needs no seam: each writer is
	// pointed at a legacy database, and the schema must be current afterwards.
	tests := []struct {
		write func(t *testing.T, store *Store) error
		name  string
	}{
		{
			name: "append_audit",
			write: func(t *testing.T, store *Store) error {
				t.Helper()
				_, err := store.AppendAudit(t.Context(), noopAuditRequest("append-invocation"))
				return err
			},
		},
		{
			name: "restart_service",
			write: func(t *testing.T, store *Store) error {
				t.Helper()
				entry, err := store.RestartServiceWithAudit(
					t.Context(), inventoryService, testIdentity("restart-invocation"),
				)
				if err == nil && entry == nil {
					t.Error("restarting a seeded service returned no audit entry")
				}
				return err
			},
		},
		{
			name: "resolve_incident",
			write: func(t *testing.T, store *Store) error {
				t.Helper()
				entry, err := store.ResolveIncidentWithAudit(
					t.Context(), inventoryIncident, testIdentity("resolve-invocation"),
				)
				if err == nil && entry == nil {
					t.Error("resolving a seeded open incident returned no audit entry")
				}
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			_, path := legacyRuntimeDatabase(t, store)

			if err := test.write(t, store); err != nil {
				t.Fatalf("write against legacy state: %v", err)
			}

			db := openFixture(t, path)
			if columns := countRows(t, db,
				"SELECT COUNT(*) FROM pragma_table_info('audit_log') WHERE name = 'schema_version'"); columns != 1 {
				t.Errorf("schema_version columns = %d, want 1: the writer did not migrate first", columns)
			}
			if indexes := countRows(t, db,
				`SELECT COUNT(*) FROM pragma_index_list('audit_log') WHERE name = ? AND "unique" = 1`,
				auditIdempotencyIndex); indexes != 1 {
				t.Errorf("unique idempotency indexes = %d, want 1: the writer did not migrate first", indexes)
			}
		})
	}
}

func TestAtomicMutationsReturnNilForUnknownOrResolvedRows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		write func(t *testing.T, store *Store) (*domain.AuditEntry, error)
		name  string
	}{
		{
			name: "restarting a service that does not exist",
			write: func(t *testing.T, store *Store) (*domain.AuditEntry, error) {
				t.Helper()
				return store.RestartServiceWithAudit(t.Context(), mustSlug("unknown"), testIdentity("invocation"))
			},
		},
		{
			name: "resolving an incident that does not exist",
			write: func(t *testing.T, store *Store) (*domain.AuditEntry, error) {
				t.Helper()
				return store.ResolveIncidentWithAudit(t.Context(), mustIncidentID("INC-999"), testIdentity("invocation"))
			},
		},
		{
			// Already resolved is a no-op, not a failure — and crucially not an
			// audit row claiming a resolution that did not happen.
			name: "resolving an incident that is already resolved",
			write: func(t *testing.T, store *Store) (*domain.AuditEntry, error) {
				t.Helper()
				return store.ResolveIncidentWithAudit(t.Context(), resolvedIncident, testIdentity("invocation"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			entry, err := test.write(t, store)
			if err != nil {
				t.Fatalf("a no-op write must not fail: %v", err)
			}
			if entry != nil {
				t.Fatalf("a no-op write produced an audit entry: %+v", entry)
			}
			if rows := countRows(t, openFixture(t, store.RuntimePath()),
				"SELECT COUNT(*) FROM audit_log"); rows != 0 {
				t.Errorf("audit rows = %d, want 0", rows)
			}
		})
	}
}

func TestAtomicMutationsDeriveContextFromLockedCurrentRows(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	fixture := openFixture(t, publishedRuntime(t, store))
	execFixture(t, fixture, "UPDATE services SET status = 'degraded' WHERE name = ?", string(inventoryService))
	execFixture(t, fixture, `
		UPDATE incidents
		SET status = 'investigating', title = 'Fresh transaction state'
		WHERE id = ?`, string(inventoryIncident))

	restart, err := store.RestartServiceWithAudit(t.Context(), inventoryService, testIdentity("invocation"))
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if restart == nil {
		t.Fatal("restarting a known service returned no audit entry")
	}
	// The context describes the rows the write is about to change, not the seed
	// they started from — that is what makes the trail worth auditing.
	for _, fragment := range []string{
		"service " + string(inventoryService) + " was degraded",
		string(inventoryIncident),
	} {
		if !strings.Contains(restart.ContextSummary(), fragment) {
			t.Errorf("restart context %q does not carry %q", restart.ContextSummary(), fragment)
		}
	}

	resolution, err := store.ResolveIncidentWithAudit(t.Context(), inventoryIncident, testIdentity("invocation"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolution == nil {
		t.Fatal("resolving an open incident returned no audit entry")
	}
	for _, fragment := range []string{"(SEV1, investigating)", "Fresh transaction state"} {
		if !strings.Contains(resolution.ContextSummary(), fragment) {
			t.Errorf("resolution context %q does not carry %q", resolution.ContextSummary(), fragment)
		}
	}
}

func TestRestartContextNamesTheAbsenceOfOpenIncidents(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	fixture := openFixture(t, publishedRuntime(t, store))
	execFixture(t, fixture, "UPDATE incidents SET status = 'resolved' WHERE service = ?", string(inventoryService))

	entry, err := store.RestartServiceWithAudit(t.Context(), inventoryService, testIdentity("invocation"))
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if entry == nil {
		t.Fatal("restarting a known service returned no audit entry")
	}
	// "none" spelled out, so a reader can tell "no open incidents" apart from a
	// truncated or malformed summary.
	if !strings.HasSuffix(entry.ContextSummary(), "open incidents: none") {
		t.Errorf("context = %q, want it to end with \"open incidents: none\"", entry.ContextSummary())
	}
}

func TestRestartReplayReturnsTheOriginalAuditWithoutReapplyingTheMutation(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	identity := testIdentity("invocation")

	first, err := store.RestartServiceWithAudit(t.Context(), inventoryService, identity)
	if err != nil {
		t.Fatalf("first restart: %v", err)
	}
	if first == nil {
		t.Fatal("the first restart returned no audit entry")
	}

	// A later event moves the service on. Re-delivering the old invocation must
	// return its evidence without overwriting this newer state — that is the
	// difference between idempotency and a retry.
	execFixture(t, openFixture(t, store.RuntimePath()),
		"UPDATE services SET status = 'degraded' WHERE name = ?", string(inventoryService))

	replay, err := store.RestartServiceWithAudit(t.Context(), inventoryService, identity)
	if err != nil {
		t.Fatalf("replayed restart: %v", err)
	}
	if replay == nil || *replay != *first {
		t.Errorf("replay = %+v, want the original entry %+v", replay, first)
	}
	service, err := store.GetService(t.Context(), inventoryService)
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	if service == nil || service.Status() != domain.ServiceStatusDegraded {
		t.Errorf("the replay re-applied the mutation: status = %+v", service)
	}
	if rows := countRows(t, openFixture(t, store.RuntimePath()), `
		SELECT COUNT(*) FROM audit_log
		WHERE invocation_id = ? AND action = ? AND target = ?`,
		identity.InvocationID, actionRestartService, string(inventoryService)); rows != 1 {
		t.Errorf("audit rows for the key = %d, want 1", rows)
	}
}

func TestResolveReplayReturnsTheOriginalAuditAfterTheIncidentIsResolved(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	identity := testIdentity("invocation")

	first, err := store.ResolveIncidentWithAudit(t.Context(), inventoryIncident, identity)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if first == nil {
		t.Fatal("the first resolve returned no audit entry")
	}
	replay, err := store.ResolveIncidentWithAudit(t.Context(), inventoryIncident, identity)
	if err != nil {
		t.Fatalf("replayed resolve: %v", err)
	}
	if replay == nil || *replay != *first {
		t.Errorf("replay = %+v, want the original entry %+v", replay, first)
	}

	// A *different* invocation over the now-resolved incident is a genuine
	// second request, and the answer is "nothing to do" rather than a second
	// audit row.
	later, err := store.ResolveIncidentWithAudit(t.Context(), inventoryIncident, testIdentity("later-invocation"))
	if err != nil {
		t.Fatalf("later resolve: %v", err)
	}
	if later != nil {
		t.Errorf("resolving an already-resolved incident returned %+v, want nil", later)
	}
	if rows := countRows(t, openFixture(t, store.RuntimePath()),
		"SELECT COUNT(*) FROM audit_log WHERE action = ? AND target = ?",
		actionResolveIncident, string(inventoryIncident)); rows != 1 {
		t.Errorf("resolve audit rows = %d, want 1", rows)
	}
}

func TestAppendAuditStampsTheSchemaVersionAndIsReplaySafe(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)

	entry, err := store.AppendAudit(t.Context(), noopAuditRequest("invocation"))
	if err != nil {
		t.Fatalf("append audit: %v", err)
	}
	if entry.SchemaVersion() != domain.CurrentAuditSchemaVersion {
		t.Errorf("schema version = %d, want %d", entry.SchemaVersion(), domain.CurrentAuditSchemaVersion)
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal the audit entry: %v", err)
	}
	var payload map[string]any
	if decodeErr := json.Unmarshal(encoded, &payload); decodeErr != nil {
		t.Fatalf("unmarshal the audit entry: %v", decodeErr)
	}
	if version, ok := payload["schema_version"].(float64); !ok || int(version) != domain.CurrentAuditSchemaVersion {
		t.Errorf("marshaled schema_version = %v, want %d", payload["schema_version"], domain.CurrentAuditSchemaVersion)
	}

	fixture := openFixture(t, store.RuntimePath())
	if stored, found := scalar[int](t, fixture,
		"SELECT schema_version FROM audit_log WHERE id = ?", entry.ID()); !found ||
		stored != domain.CurrentAuditSchemaVersion {
		t.Errorf("stored schema_version = %d (found %t), want %d",
			stored, found, domain.CurrentAuditSchemaVersion)
	}
	if dflt, found := scalar[string](t, fixture,
		"SELECT dflt_value FROM pragma_table_info('audit_log') WHERE name = 'schema_version'"); !found ||
		dflt != "1" {
		t.Errorf("schema_version default = %q (found %t), want \"1\"", dflt, found)
	}

	// A redelivery carrying a different rationale must not rewrite the record
	// of what actually happened.
	changed := noopAuditRequest("invocation")
	changed.Rationale = "a changed replay payload must not replace the first row"
	replay, err := store.AppendAudit(t.Context(), changed)
	if err != nil {
		t.Fatalf("replayed append: %v", err)
	}
	if replay != entry {
		t.Errorf("replay = %+v, want the original entry %+v", replay, entry)
	}
}

func TestTheUniqueIndexRejectsARawDuplicateAuditRow(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	entry, err := store.AppendAudit(t.Context(), noopAuditRequest("invocation"))
	if err != nil {
		t.Fatalf("append audit: %v", err)
	}

	// The application-level replay check is a courtesy; the index is the
	// guarantee. A writer that bypasses this package entirely still cannot
	// duplicate a logical write.
	fixture := openFixture(t, store.RuntimePath())
	_, err = fixture.ExecContext(t.Context(), `
		INSERT INTO audit_log
			(ts, actor, approved_by, rationale, context_summary, session_id, invocation_id, action, target, detail)
		SELECT ts, actor, approved_by, rationale, context_summary, session_id, invocation_id, action, target, detail
		FROM audit_log
		WHERE id = ?`, entry.ID())
	if err == nil {
		t.Fatal("a raw duplicate insert succeeded")
	}
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Errorf("duplicate insert failed for the wrong reason: %v", err)
	}
}

func TestRestartContextIsReadAfterAcquiringTheWriteLock(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	path := publishedRuntime(t, store)

	observed := false
	store.onRestartContext = func() {
		observed = true
		// A competitor with no busy timeout fails immediately if — and only if
		// — the write lock is already held. Reading the decision context before
		// BEGIN IMMEDIATE would let this write through, and the audit trail
		// would then describe a state another writer had already left.
		dsn, err := dataSourceName(path, "_pragma=busy_timeout(0)")
		if err != nil {
			t.Errorf("build the competitor DSN: %v", err)
			return
		}
		competitor, err := sql.Open(sqliteDriver, dsn)
		if err != nil {
			t.Errorf("open the competitor: %v", err)
			return
		}
		defer func() {
			if closeErr := competitor.Close(); closeErr != nil {
				t.Errorf("close the competitor: %v", closeErr)
			}
		}()
		_, err = competitor.ExecContext(t.Context(),
			"UPDATE services SET status = 'degraded' WHERE name = ?", string(inventoryService))
		if err == nil {
			t.Error("a competing write succeeded while the restart transaction should have held the lock")
			return
		}
		if !strings.Contains(err.Error(), "locked") {
			t.Errorf("the competing write failed for the wrong reason: %v", err)
		}
	}

	entry, err := store.RestartServiceWithAudit(t.Context(), inventoryService, testIdentity("invocation"))
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if !observed {
		t.Fatal("the restart never read its decision context")
	}
	if entry == nil {
		t.Fatal("restarting a known service returned no audit entry")
	}
	// The seeded status, proving the context came from the locked rows and not
	// from the competitor's rejected write.
	if want := "service " + string(inventoryService) + " was down"; !strings.Contains(entry.ContextSummary(), want) {
		t.Errorf("context = %q, want it to carry %q", entry.ContextSummary(), want)
	}
}

// idlessResult is a driver result that accepted the insert but produced no row
// id. It is the Go counterpart of Python's cursor.lastrowid being None: no real
// driver does this on demand, and the branch that handles it is a database
// boundary worth covering.
type idlessResult struct{}

func (idlessResult) LastInsertId() (int64, error) { return 0, errors.New("no row id available") }
func (idlessResult) RowsAffected() (int64, error) { return 1, nil }

// idlessExecer accepts any statement and always answers with idlessResult.
type idlessExecer struct{}

func (idlessExecer) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return idlessResult{}, nil
}

func TestAppendAuditRejectsAMissingRowID(t *testing.T) {
	t.Parallel()

	_, err := appendAudit(t.Context(), idlessExecer{}, noopAuditRequest("invocation"))
	if !errors.Is(err, ErrDataAccess) {
		t.Fatalf("expected an ErrDataAccess failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "audit entry id") {
		t.Errorf("error does not name the missing id: %v", err)
	}
}

func TestAppendAuditRefusesAnEntryTheDomainRejects(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	request := noopAuditRequest("invocation")
	// The rationale is the human justification an auditor relies on, and the
	// domain bounds its length. An entry that cannot be represented must not
	// reach the trail at all.
	request.Rationale = strings.Repeat("x", domain.MaxAuditRationaleLength+1)

	_, err := store.AppendAudit(t.Context(), request)
	if !errors.Is(err, ErrDataAccess) {
		t.Fatalf("expected an ErrDataAccess failure, got %v", err)
	}
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("the domain's rejection was not preserved in the chain: %v", err)
	}
	// The insert already ran inside the transaction, so the rollback is what
	// keeps the row out of the trail.
	if rows := countRows(t, openFixture(t, store.RuntimePath()),
		"SELECT COUNT(*) FROM audit_log"); rows != 0 {
		t.Errorf("audit rows = %d, want 0: the rejected entry was committed", rows)
	}
}

func TestWritesLeaveTheCommittedSeedUntouched(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	seed := takeSnapshot(t, store.SeedPath())

	if _, err := store.AppendAudit(t.Context(), noopAuditRequest("append-invocation")); err != nil {
		t.Fatalf("append audit: %v", err)
	}
	if _, err := store.RestartServiceWithAudit(
		t.Context(), inventoryService, testIdentity("restart-invocation"),
	); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if _, err := store.ResolveIncidentWithAudit(
		t.Context(), inventoryIncident, testIdentity("resolve-invocation"),
	); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The course invariant: the dataset is input. Every mutation landed in the
	// state directory, and the byte-reproducible seed is exactly as committed.
	seed.assertUnchanged(t, store.SeedPath())
}

func TestReplayLookupRejectsAnUnrepresentableStoredEntry(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	path := publishedRuntime(t, store)
	// audit_log puts no constraint on rationale, so a foreign writer can store
	// an empty one. The replay lookup parses what it finds, and an entry it
	// cannot represent must stop the write rather than be silently discarded —
	// discarding it would let the same logical write run twice.
	execFixture(t, openFixture(t, path), `
		INSERT INTO audit_log
			(schema_version, ts, actor, approved_by, rationale, context_summary,
			 session_id, invocation_id, action, target, detail)
		VALUES (1, '2026-07-31T08:00:00Z', 'foreign-agent', 'engineer', '', 'context',
				'session', 'invocation', 'noop', ?, 'detail')`, string(checkoutService))

	_, err := store.AppendAudit(t.Context(), noopAuditRequest("invocation"))
	if !errors.Is(err, ErrDataAccess) {
		t.Fatalf("expected an ErrDataAccess failure, got %v", err)
	}
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("the domain's rejection was not preserved in the chain: %v", err)
	}
	if !strings.Contains(err.Error(), "is not valid") {
		t.Errorf("error does not describe the unrepresentable entry: %v", err)
	}
}
