package data

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// nocaseIndexSQL recreates the idempotency index with a case-insensitive first
// column. It is unique and non-partial, so only the collation check rejects it —
// and it must, because two invocation ids differing in case would then collide
// and a genuine second write would be swallowed as a replay.
const nocaseIndexSQL = `CREATE UNIQUE INDEX uq_audit_log_idempotency
	ON audit_log (invocation_id COLLATE NOCASE, action, target)`

// partialIndexSQL recreates it with a WHERE clause. Rows outside the predicate
// are unconstrained, so duplicates could still land there.
const partialIndexSQL = `CREATE UNIQUE INDEX uq_audit_log_idempotency
	ON audit_log (invocation_id, action, target)
	WHERE action = 'restart_service'`

// runtimeDatabaseWithIndex publishes runtime state, replaces the idempotency
// index with the given definition, and returns the runtime path.
func runtimeDatabaseWithIndex(t *testing.T, store *Store, definition string) string {
	t.Helper()
	path := publishedRuntime(t, store)
	db := openFixture(t, path)
	execFixture(t, db, "DROP INDEX uq_audit_log_idempotency")
	execFixture(t, db, definition)
	return path
}

func TestProbeRefusesUninitializedStateWithoutCreatingIt(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)

	_, err := store.ProbeRuntimeDatabase(t.Context())
	if !errors.Is(err, ErrDataAccess) || !strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("expected a not-initialized refusal, got %v", err)
	}
	// The probe is the state *reader*. Only DBPath owns publication, and a
	// health check that quietly created the thing it is checking would report
	// success on a machine that has none.
	if _, err := os.Stat(store.RuntimePath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the probe created runtime state: stat error %v", err)
	}
}

func TestProbeAcceptsPublishedStateWithoutTouchingIt(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	path := publishedRuntime(t, store)
	before := takeSnapshot(t, path)

	probed, err := store.ProbeRuntimeDatabase(t.Context())
	if err != nil {
		t.Fatalf("probe freshly published state: %v", err)
	}
	if probed != path {
		t.Errorf("probe returned %q, want %q", probed, path)
	}
	before.assertUnchanged(t, path)
}

func TestProbeWrapsUnreadableState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		prepare func(t *testing.T, store *Store) context.Context
		name    string
	}{
		{
			name: "content is not a database",
			prepare: func(t *testing.T, store *Store) context.Context {
				t.Helper()
				if err := os.MkdirAll(store.StateDir(), stateDirPerm); err != nil {
					t.Fatalf("create the state directory: %v", err)
				}
				if err := os.WriteFile(store.RuntimePath(), []byte("not a SQLite database"), 0o600); err != nil {
					t.Fatalf("write the corrupt state: %v", err)
				}
				return t.Context()
			},
		},
		{
			// The Go counterpart of Python monkeypatching sqlite3.connect to
			// raise: a canceled context makes the connection genuinely
			// unobtainable, and the probe must report that as a probe failure
			// rather than as a verdict about the schema.
			name: "the connection cannot be established",
			prepare: func(t *testing.T, store *Store) context.Context {
				t.Helper()
				publishedRuntime(t, store)
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			ctx := test.prepare(t, store)
			before := takeSnapshot(t, store.RuntimePath())

			_, err := store.ProbeRuntimeDatabase(ctx)
			if !errors.Is(err, ErrDataAccess) || !strings.Contains(err.Error(), "read-only probe failed") {
				t.Fatalf("expected a probe failure, got %v", err)
			}
			before.assertUnchanged(t, store.RuntimePath())
		})
	}
}

func TestProbeRejectsAnIncompleteSchema(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	if err := os.MkdirAll(store.StateDir(), stateDirPerm); err != nil {
		t.Fatalf("create the state directory: %v", err)
	}
	db := openFixture(t, store.RuntimePath())
	execFixture(t, db, "CREATE TABLE services (name TEXT PRIMARY KEY)")

	_, err := store.ProbeRuntimeDatabase(t.Context())
	if !errors.Is(err, ErrDataAccess) || !strings.Contains(err.Error(), "missing required tables") {
		t.Fatalf("expected a missing-tables refusal, got %v", err)
	}
	// Both missing tables are named, not just the first one found: an operator
	// restoring a snapshot needs the whole list in one pass.
	for _, table := range []string{"audit_log", "incidents"} {
		if !strings.Contains(err.Error(), table) {
			t.Errorf("error does not name the missing table %q: %v", table, err)
		}
	}
}

func TestProbeRejectsUnpreparedSchemasWithoutRepairingThem(t *testing.T) {
	t.Parallel()
	tests := []struct {
		prepare func(t *testing.T, store *Store) string
		// verify re-checks the database afterwards: the probe must leave the
		// defect exactly as it found it.
		verify func(t *testing.T, db *sql.DB)
		name   string
		// wantMessage is the part of the refusal that tells the operator which
		// half of the audit contract is broken.
		wantMessage string
	}{
		{
			name: "legacy schema is neither migrated nor accepted",
			prepare: func(t *testing.T, store *Store) string {
				t.Helper()
				_, path := legacyRuntimeDatabase(t, store)
				return path
			},
			wantMessage: "audit schema is not prepared",
			verify: func(t *testing.T, db *sql.DB) {
				t.Helper()
				if columns := countRows(t, db,
					"SELECT COUNT(*) FROM pragma_table_info('audit_log') WHERE name = 'schema_version'"); columns != 0 {
					t.Errorf("the probe added the schema_version column: %d found", columns)
				}
			},
		},
		{
			name: "a NOCASE index is rejected on its collation",
			prepare: func(t *testing.T, store *Store) string {
				t.Helper()
				return runtimeDatabaseWithIndex(t, store, nocaseIndexSQL)
			},
			wantMessage: "ascending BINARY unique index",
			verify: func(t *testing.T, db *sql.DB) {
				t.Helper()
				key := indexKeyColumns(t, db)
				if len(key) == 0 || key[0].Coll != "NOCASE" {
					t.Errorf("the probe rewrote the index: %+v", key)
				}
			},
		},
		{
			name: "missing append-only triggers are not recreated",
			prepare: func(t *testing.T, store *Store) string {
				t.Helper()
				path := publishedRuntime(t, store)
				db := openFixture(t, path)
				execFixture(t, db, "DROP TRIGGER audit_log_no_update")
				execFixture(t, db, "DROP TRIGGER audit_log_no_delete")
				return path
			},
			wantMessage: "append-only triggers",
			verify: func(t *testing.T, db *sql.DB) {
				t.Helper()
				if names := triggerNames(t, db); len(names) != 0 {
					t.Errorf("the probe recreated triggers: %v", names)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			path := test.prepare(t, store)
			before := takeSnapshot(t, path)

			_, err := store.ProbeRuntimeDatabase(t.Context())
			if !errors.Is(err, ErrDataAccess) || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("expected a refusal mentioning %q, got %v", test.wantMessage, err)
			}
			before.assertUnchanged(t, path)
			test.verify(t, openFixture(t, path))
		})
	}
}

func TestUnsupportedAuditSchemaIsRejectedOnEveryDoor(t *testing.T) {
	t.Parallel()
	fixtures := []struct {
		apply func(t *testing.T, db *sql.DB)
		name  string
	}{
		{
			name: "a version this binary is too old to read",
			apply: func(t *testing.T, db *sql.DB) {
				t.Helper()
				execFixture(t, db, `
					INSERT INTO audit_log
						(schema_version, ts, actor, approved_by, rationale, context_summary,
						 session_id, invocation_id, action, target, detail)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					domain.CurrentAuditSchemaVersion+1, "2026-07-31T08:00:00Z", "future-agent", "engineer",
					"future approval", "future context", "future-session", "future-invocation",
					actionRestartService, string(inventoryService), "future detail")
			},
		},
		{
			// The CHECK constraint is suspended to write a row SQLite would
			// otherwise refuse. A snapshot from a different application could
			// carry exactly this, and the guard has to reject on the stored
			// *type*, not only on the numeric range.
			name: "a version that is not even an integer",
			apply: func(t *testing.T, db *sql.DB) {
				t.Helper()
				execFixture(t, db, "PRAGMA ignore_check_constraints = ON")
				execFixture(t, db, `
					INSERT INTO audit_log
						(schema_version, ts, actor, approved_by, rationale, context_summary,
						 session_id, invocation_id, action, target, detail)
					VALUES ('future', '2026-07-31T08:00:00Z', 'future-agent', 'engineer',
							'invalid schema fixture', 'future context', 'invalid-session',
							'invalid-invocation', ?, ?, 'future detail')`,
					actionRestartService, string(inventoryService))
			},
		},
	}
	doors := []struct {
		call func(t *testing.T, store *Store) error
		name string
	}{
		{
			name: "probe",
			call: func(t *testing.T, store *Store) error {
				t.Helper()
				_, err := store.ProbeRuntimeDatabase(t.Context())
				return err
			},
		},
		{
			name: "read",
			call: func(t *testing.T, store *Store) error {
				t.Helper()
				_, err := store.ListIncidents(t.Context(), IncidentFilter{})
				return err
			},
		},
		{
			name: "migrate",
			call: func(t *testing.T, store *Store) error {
				t.Helper()
				_, err := store.PrepareRuntimeDatabase(t.Context())
				return err
			},
		},
	}

	const guidance = "Upgrade the application or select a compatible snapshot"
	for _, fixture := range fixtures {
		for _, door := range doors {
			t.Run(fixture.name+"/"+door.name, func(t *testing.T) {
				t.Parallel()
				store := newTestStore(t)
				path := publishedRuntime(t, store)
				fixture.apply(t, openFixture(t, path))
				before := takeSnapshot(t, path)

				err := door.call(t, store)
				if !errors.Is(err, ErrDataAccess) || !strings.Contains(err.Error(), guidance) {
					t.Fatalf("expected the unsupported-schema guidance, got %v", err)
				}
				// Refusing has to be inert: an operator who reruns the agent
				// against a newer snapshot must find it exactly as they left it.
				before.assertUnchanged(t, path)
			})
		}
	}
}

func TestPrepareMigratesLegacyStateWithoutTouchingTheSeed(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	seed := takeSnapshot(t, store.SeedPath())
	fixture, path := legacyRuntimeDatabase(t, store)
	insertLegacyAudit(t, fixture, "legacy-invocation", "first delivery")

	prepared, err := store.PrepareRuntimeDatabase(t.Context())
	if err != nil {
		t.Fatalf("migrate legacy runtime state: %v", err)
	}
	if prepared != path {
		t.Errorf("prepare returned %q, want %q", prepared, path)
	}

	migrated := openFixture(t, path)
	// The pre-existing row is backfilled with the version this binary writes,
	// which is the correct reading of a row written before the column existed.
	if version, found := scalar[int](t, migrated, "SELECT schema_version FROM audit_log"); !found ||
		version != domain.CurrentAuditSchemaVersion {
		t.Errorf("legacy row schema_version = %d (found %t), want %d",
			version, found, domain.CurrentAuditSchemaVersion)
	}
	// SQLite reports a column default as text, so the contract is the string.
	if dflt, found := scalar[string](t, migrated,
		"SELECT dflt_value FROM pragma_table_info('audit_log') WHERE name = 'schema_version'"); !found || dflt != "1" {
		t.Errorf("schema_version default = %q (found %t), want \"1\"", dflt, found)
	}
	if unique, found := scalar[int](t, migrated,
		`SELECT "unique" FROM pragma_index_list('audit_log') WHERE name = ?`, auditIdempotencyIndex); !found ||
		unique != 1 {
		t.Errorf("idempotency index unique = %d (found %t), want 1", unique, found)
	}
	if key := indexKeyColumns(t, migrated); !slices.Equal(key, auditIdempotencyKey) {
		t.Errorf("index key = %+v, want %+v", key, auditIdempotencyKey)
	}
	// The committed dataset is input, never output. A migration that reached it
	// would break the repository's byte-for-byte reproducibility gate.
	seed.assertUnchanged(t, store.SeedPath())
}

func TestPrepareRejectsDuplicateLegacyKeysAtomically(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	fixture, path := legacyRuntimeDatabase(t, store)
	insertLegacyAudit(t, fixture, "duplicate-invocation", "first delivery")
	insertLegacyAudit(t, fixture, "duplicate-invocation", "second delivery")

	_, err := store.PrepareRuntimeDatabase(t.Context())
	if !errors.Is(err, ErrDataAccess) || !strings.Contains(err.Error(), "duplicate audit idempotency key") {
		t.Fatalf("expected a duplicate-key refusal, got %v", err)
	}
	// The message has to carry enough for an operator to find the rows: which
	// key, how many, and what to do about it.
	for _, fragment := range []string{"duplicate-invocation", "2 rows", "reconcile"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error does not carry %q: %v", fragment, err)
		}
	}

	// The refusal happens mid-migration, after the column was added, so the
	// rollback is the thing being tested: neither half of the change survives.
	after := openFixture(t, path)
	if columns := countRows(t, after,
		"SELECT COUNT(*) FROM pragma_table_info('audit_log') WHERE name = 'schema_version'"); columns != 0 {
		t.Errorf("the schema_version column survived the rollback: %d found", columns)
	}
	if indexes := countRows(t, after,
		`SELECT COUNT(*) FROM pragma_index_list('audit_log') WHERE name = ?`, auditIdempotencyIndex); indexes != 0 {
		t.Errorf("the idempotency index survived the rollback: %d found", indexes)
	}
}

func TestPrepareIsIdempotentOnACurrentSchema(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)

	first, err := store.PrepareRuntimeDatabase(t.Context())
	if err != nil {
		t.Fatalf("first migration: %v", err)
	}
	second, err := store.PrepareRuntimeDatabase(t.Context())
	if err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if second != first {
		t.Errorf("second migration returned %q, want %q", second, first)
	}

	db := openFixture(t, first)
	if columns := countRows(t, db,
		"SELECT COUNT(*) FROM pragma_table_info('audit_log') WHERE name = 'schema_version'"); columns != 1 {
		t.Errorf("schema_version columns = %d, want 1", columns)
	}
	if indexes := countRows(t, db,
		`SELECT COUNT(*) FROM pragma_index_list('audit_log') WHERE name = ? AND "unique" = 1`,
		auditIdempotencyIndex); indexes != 1 {
		t.Errorf("unique idempotency indexes = %d, want 1", indexes)
	}
}

func TestPrepareRecreatesMissingAppendOnlyTriggers(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	path, err := store.PrepareRuntimeDatabase(t.Context())
	if err != nil {
		t.Fatalf("first migration: %v", err)
	}
	fixture := openFixture(t, path)
	execFixture(t, fixture, "DROP TRIGGER audit_log_no_update")
	execFixture(t, fixture, "DROP TRIGGER audit_log_no_delete")

	// Recreating is safe where replacing would not be: a missing trigger is a
	// strictly weaker database than the contract, so restoring it only tightens.
	if _, err := store.PrepareRuntimeDatabase(t.Context()); err != nil {
		t.Fatalf("re-migrate after the triggers were dropped: %v", err)
	}
	names := triggerNames(t, openFixture(t, path))
	for _, trigger := range auditTriggers {
		if _, ok := names[trigger.Name]; !ok {
			t.Errorf("trigger %q was not recreated; have %v", trigger.Name, names)
		}
	}
	if len(names) != len(auditTriggers) {
		t.Errorf("trigger count = %d, want %d: %v", len(names), len(auditTriggers), names)
	}

	// And the recreated trigger really is append-only, not merely present.
	execFixture(t, fixture, `
		INSERT INTO audit_log
			(schema_version, ts, actor, approved_by, rationale, context_summary,
			 session_id, invocation_id, action, target, detail)
		VALUES (1, 'ts', 'actor', 'engineer', 'why', 'context', 'session', 'invocation', 'noop', 'target', 'detail')`)
	if _, err := fixture.ExecContext(t.Context(), "DELETE FROM audit_log"); err == nil {
		t.Error("the recreated trigger allowed a delete from audit_log")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("delete was rejected for the wrong reason: %v", err)
	}
}

func TestPrepareRejectsAnUnexpectedIndexDefinition(t *testing.T) {
	t.Parallel()
	tests := []struct {
		verify     func(t *testing.T, db *sql.DB)
		name       string
		definition string
	}{
		{
			name:       "partial index",
			definition: partialIndexSQL,
			verify: func(t *testing.T, db *sql.DB) {
				t.Helper()
				if partial, found := scalar[int](t, db,
					`SELECT partial FROM pragma_index_list('audit_log') WHERE name = ?`,
					auditIdempotencyIndex); !found || partial != 1 {
					t.Errorf("partial = %d (found %t), want 1: the migration rewrote the index", partial, found)
				}
			},
		},
		{
			name:       "NOCASE index",
			definition: nocaseIndexSQL,
			verify: func(t *testing.T, db *sql.DB) {
				t.Helper()
				key := indexKeyColumns(t, db)
				collations := make([]string, 0, len(key))
				for _, column := range key {
					collations = append(collations, column.Coll)
				}
				if want := []string{"NOCASE", "BINARY", "BINARY"}; !slices.Equal(collations, want) {
					t.Errorf("collations = %v, want %v: the migration rewrote the index", collations, want)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			path := runtimeDatabaseWithIndex(t, store, test.definition)

			_, err := store.PrepareRuntimeDatabase(t.Context())
			if !errors.Is(err, ErrDataAccess) || !strings.Contains(err.Error(), "unexpected definition") {
				t.Fatalf("expected an unexpected-definition refusal, got %v", err)
			}
			if !strings.Contains(err.Error(), auditIdempotencyIndex) {
				t.Errorf("error does not name the index: %v", err)
			}
			// Never repaired, only refused: somebody created this index, and
			// dropping it would destroy whatever they were protecting.
			test.verify(t, openFixture(t, path))
		})
	}
}

func TestPrepareRejectsAConflictingTriggerBody(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	path := publishedRuntime(t, store)
	fixture := openFixture(t, path)
	execFixture(t, fixture, "DROP TRIGGER audit_log_no_delete")
	execFixture(t, fixture, `CREATE TRIGGER audit_log_no_delete
		BEFORE DELETE ON audit_log
		BEGIN
			SELECT RAISE(ABORT, 'someone else owns this trigger');
		END`)

	_, err := store.PrepareRuntimeDatabase(t.Context())
	if !errors.Is(err, ErrDataAccess) || !strings.Contains(err.Error(), "unexpected definition") {
		t.Fatalf("expected an unexpected-definition refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), "audit_log_no_delete") {
		t.Errorf("error does not name the trigger: %v", err)
	}
	body, found := scalar[string](t, fixture,
		`SELECT sql FROM sqlite_schema WHERE type = 'trigger' AND name = 'audit_log_no_delete'`)
	if !found || !strings.Contains(body, "someone else owns this trigger") {
		t.Errorf("the migration replaced a trigger it did not write: %q (found %t)", body, found)
	}
}

func TestPrepareAcceptsAReindentedTriggerBody(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	path := publishedRuntime(t, store)
	fixture := openFixture(t, path)
	execFixture(t, fixture, "DROP TRIGGER audit_log_no_update")
	// Same statement, different whitespace. SQLite stores a trigger's source
	// text verbatim, so the comparison has to forgive formatting — and only
	// formatting.
	execFixture(t, fixture,
		"CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log "+
			"BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END")

	if _, err := store.PrepareRuntimeDatabase(t.Context()); err != nil {
		t.Fatalf("a reindented but identical trigger must be accepted: %v", err)
	}
}

func TestNormalizedSchemaSQLForgivesOnlyFormatting(t *testing.T) {
	t.Parallel()
	reference := normalizedSchemaSQL(auditTriggers[0].SQL)
	tests := []struct {
		name string
		sql  string
		same bool
	}{
		{name: "identical", sql: auditTriggers[0].SQL, same: true},
		{
			name: "collapsed onto one line",
			sql: "CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log " +
				"BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END",
			same: true,
		},
		{
			name: "different case",
			sql: "create trigger audit_log_no_update before update on audit_log " +
				"begin select raise(abort, 'audit_log is append-only'); end",
			same: true,
		},
		{
			name: "a different table",
			sql: "CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON incidents " +
				"BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END",
			same: false,
		},
		{
			name: "a weakened action",
			sql: "CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log " +
				"BEGIN SELECT 1; END",
			same: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizedSchemaSQL(test.sql) == reference; got != test.same {
				t.Errorf("normalized match = %t, want %t for %q", got, test.same, test.sql)
			}
		})
	}
}

func TestProbeRejectsACorruptedDatabaseFile(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	path := publishedRuntime(t, store)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the published state: %v", err)
	}
	// Keep the SQLite header so the file still opens, and corrupt a page so the
	// integrity check is the thing that catches it.
	copy(content[len(content)/2:], strings.Repeat("\xff", 512))
	if writeErr := os.WriteFile(path, content, 0o600); writeErr != nil {
		t.Fatalf("write the corrupted state: %v", writeErr)
	}

	_, err = store.ProbeRuntimeDatabase(t.Context())
	if !errors.Is(err, ErrDataAccess) {
		t.Fatalf("expected an ErrDataAccess failure, got %v", err)
	}
	// Either verdict is honest — a torn page can fail the integrity check or
	// make the file unreadable outright — but it must never be reported as a
	// healthy database.
	if !strings.Contains(err.Error(), "integrity check failed") &&
		!strings.Contains(err.Error(), "read-only probe failed") {
		t.Errorf("corruption was not reported as such: %v", err)
	}
}

func TestBindRequiresAnExactColumnMatch(t *testing.T) {
	t.Parallel()
	var name, owner string
	fields := map[string]any{"name": &name, "owner": &owner}
	tests := []struct {
		name    string
		wantErr string
		columns []string
	}{
		{name: "exact match", columns: []string{"name", "owner"}},
		{name: "reordered columns still bind by name", columns: []string{"owner", "name"}},
		{name: "an added column", columns: []string{"name", "owner", "tier"}, wantErr: "expected 2 columns, got 3"},
		{name: "a dropped column", columns: []string{"name"}, wantErr: "expected 2 columns, got 1"},
		{name: "a renamed column", columns: []string{"name", "team"}, wantErr: `unexpected column "team"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			destinations, err := bind(test.columns, fields)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("expected an error containing %q, got %v", test.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("bind: %v", err)
			}
			for position, column := range test.columns {
				if destinations[position] != fields[column] {
					t.Errorf("column %q bound to the wrong destination", column)
				}
			}
		})
	}
}

func TestOpenForReadPrefersRuntimeStateOverTheSeed(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)

	// With nothing published, reads fall back to the committed seed — and must
	// not create state on the way.
	path, err := store.readPath()
	if err != nil {
		t.Fatalf("resolve the read path: %v", err)
	}
	if path != store.SeedPath() {
		t.Errorf("read path = %q, want the seed %q", path, store.SeedPath())
	}
	if _, statErr := os.Stat(store.StateDir()); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("resolving a read path created the state directory: %v", statErr)
	}

	publishedRuntime(t, store)
	path, err = store.readPath()
	if err != nil {
		t.Fatalf("resolve the read path after publication: %v", err)
	}
	if path != store.RuntimePath() {
		t.Errorf("read path = %q, want the runtime copy %q", path, store.RuntimePath())
	}
}

func TestReadPathFailsWhenNeitherDatabaseExists(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := New(Config{DataDir: filepath.Join(root, "missing"), StateDir: filepath.Join(root, "state")})

	_, err := store.readPath()
	if !errors.Is(err, ErrDataAccess) || !strings.Contains(err.Error(), "Database is missing") {
		t.Fatalf("expected a missing-database failure, got %v", err)
	}
	// The base name only: a read failure surfaces through tool output the model
	// sees, and an absolute host path there is a needless disclosure.
	if strings.Contains(err.Error(), root) {
		t.Errorf("the read failure leaked an absolute path: %v", err)
	}
}

func TestReadAndProbeRejectASymlinkedRuntimeInsteadOfFollowingOrFallingBack(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	if err := os.MkdirAll(store.StateDir(), stateDirPerm); err != nil {
		t.Fatalf("create state directory: %v", err)
	}
	if err := os.Symlink(store.SeedPath(), store.RuntimePath()); err != nil {
		t.Skipf("create a runtime symlink on this platform: %v", err)
	}
	for name, operation := range map[string]func() error{
		"read path": func() error { _, err := store.readPath(); return err },
		"probe":     func() error { _, err := store.ProbeRuntimeDatabase(t.Context()); return err },
	} {
		t.Run(name, func(t *testing.T) {
			err := operation()
			if !errors.Is(err, ErrDataAccess) || !strings.Contains(err.Error(), "regular file") {
				t.Fatalf("operation error = %v, want no-follow regular-file refusal", err)
			}
		})
	}
}

func TestPrepareRejectsAReplacementBetweenInspectionAndSQLiteConnect(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	runtimePath := publishedRuntime(t, store)
	outside := filepath.Join(filepath.Dir(store.StateDir()), "outside.db")
	copyTestFile(t, runtimePath, outside)
	outsideBefore := takeSnapshot(t, outside)
	store.beforeDatabaseConnect = func(path string) {
		store.beforeDatabaseConnect = nil
		if err := os.Remove(path); err != nil {
			t.Fatalf("replace runtime database: %v", err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Skipf("replace runtime database with a symlink on this platform: %v", err)
		}
	}

	_, err := store.PrepareRuntimeDatabase(t.Context())
	if !errors.Is(err, ErrDataAccess) || !strings.Contains(err.Error(), "changed while") {
		t.Fatalf("PrepareRuntimeDatabase() error = %v, want replacement-race refusal", err)
	}
	outsideBefore.assertUnchanged(t, outside)
}

func TestPrepareRejectsAnUnexpectedVersionColumnDefinition(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	path := publishedRuntime(t, store)
	fixture := openFixture(t, path)
	// A column of the right name but the wrong declaration. Nothing about it is
	// safe to "fix" automatically: a TEXT version column means some other writer
	// has its own idea of what a version is.
	execFixture(t, fixture, "ALTER TABLE audit_log DROP COLUMN schema_version")
	execFixture(t, fixture, "ALTER TABLE audit_log ADD COLUMN schema_version TEXT NOT NULL DEFAULT 'one'")

	_, err := store.PrepareRuntimeDatabase(t.Context())
	if !errors.Is(err, ErrDataAccess) {
		t.Fatalf("expected an ErrDataAccess failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "schema_version has an unexpected definition") {
		t.Errorf("error does not describe the bad column: %v", err)
	}
	if typ, found := scalar[string](t, fixture,
		"SELECT type FROM pragma_table_info('audit_log') WHERE name = 'schema_version'"); !found || typ != "TEXT" {
		t.Errorf("column type = %q (found %t), want TEXT: the migration rewrote it", typ, found)
	}
}

func TestReadsWrapDriverFailuresWithTheDatabaseName(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	path := publishedRuntime(t, store)
	// audit_log stays intact so the future-schema guard passes and the failure
	// lands where it is being tested: on the query itself.
	execFixture(t, openFixture(t, path), "DROP TABLE incidents")

	_, err := store.ListIncidents(t.Context(), IncidentFilter{})
	if !errors.Is(err, ErrDataAccess) {
		t.Fatalf("expected an ErrDataAccess failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "SQLite read failed for "+databaseName) {
		t.Errorf("error does not name the failing read: %v", err)
	}
}

func TestWritesWrapDriverFailuresWithTheDatabaseName(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	path := publishedRuntime(t, store)
	execFixture(t, openFixture(t, path), "DROP TABLE services")

	_, err := store.RestartServiceWithAudit(t.Context(), inventoryService, testIdentity("invocation"))
	if !errors.Is(err, ErrDataAccess) {
		t.Fatalf("expected an ErrDataAccess failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "SQLite operation failed for "+databaseName) {
		t.Errorf("error does not name the failing write: %v", err)
	}
}
