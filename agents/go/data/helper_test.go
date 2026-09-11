package data

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// repositoryDataset is the committed dataset, relative to this package.
//
// `go test` runs with the working directory set to the package directory, so
// this resolves to agents/data. Every test copies it before touching it: the
// committed incidents.db is byte-reproducible and gated by a `cmp` against a
// rebuild, so a test that wrote to it would break the repository, not just
// itself.
const repositoryDataset = "../../data"

// The dataset identifiers come from the domain vocabulary rather than from
// string literals here, so a renamed domain carries these along with it — the
// same discipline the Python suite kept by importing REFERENCE_DOMAIN.
var (
	checkoutService   = mustSlug(domain.Reference().Services.Checkout)
	inventoryService  = mustSlug(domain.Reference().Services.Inventory)
	inventoryIncident = mustIncidentID(domain.Reference().Incidents.InventoryDown)
	resolvedIncident  = mustIncidentID(domain.Reference().Incidents.PaymentsErrors)
)

// mustSlug and mustIncidentID parse identifiers that come from the vocabulary
// itself. A failure is a broken vocabulary rather than a broken test, so they
// panic during package initialization instead of failing one case.
func mustSlug(value string) domain.Slug {
	slug, err := domain.ParseSlug(value)
	if err != nil {
		panic(err)
	}
	return slug
}

func mustIncidentID(value string) domain.IncidentID {
	id, err := domain.ParseIncidentID(value)
	if err != nil {
		panic(err)
	}
	return id
}

// testIdentity is the fixed approval identity every write test records.
func testIdentity(invocationID string) AuditIdentity {
	return AuditIdentity{
		Actor:        "test",
		ApprovedBy:   "engineer",
		Rationale:    "test approval",
		SessionID:    "session",
		InvocationID: invocationID,
	}
}

// newTestStore copies the committed dataset into a temporary directory and
// returns a Store pointed at it.
//
// The state directory is deliberately *not* created: every test therefore
// starts from "nothing published", which is the state a fresh checkout is in,
// and the publication path is exercised for real rather than stubbed.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.CopyFS(dataDir, os.DirFS(repositoryDataset)); err != nil {
		t.Fatalf("copy the committed dataset: %v", err)
	}
	return New(Config{DataDir: dataDir, StateDir: filepath.Join(root, "state")})
}

// openFixture opens a database read-write for test setup and assertions. It is
// deliberately not one of the package's own connections: a fixture that used
// the code under test could not contradict it.
func openFixture(t *testing.T, path string) *sql.DB {
	t.Helper()
	dsn, err := dataSourceName(path, "_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("build the fixture DSN: %v", err)
	}
	db, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		t.Fatalf("open the fixture database: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the fixture database: %v", err)
		}
	})
	return db
}

// execFixture runs one setup statement and fails the test if it does not apply.
func execFixture(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatalf("fixture statement %q: %v", query, err)
	}
}

// publishedRuntime publishes runtime state and returns its path.
func publishedRuntime(t *testing.T, store *Store) string {
	t.Helper()
	path, err := store.DBPath()
	if err != nil {
		t.Fatalf("publish runtime state: %v", err)
	}
	return path
}

// legacyRuntimeDatabase publishes runtime state and rewinds its audit schema to
// the shape that predates the idempotency contract: no unique index and no
// schema_version column.
func legacyRuntimeDatabase(t *testing.T, store *Store) (*sql.DB, string) {
	t.Helper()
	path := publishedRuntime(t, store)
	db := openFixture(t, path)
	execFixture(t, db, "DROP INDEX IF EXISTS uq_audit_log_idempotency")
	execFixture(t, db, "ALTER TABLE audit_log DROP COLUMN schema_version")
	return db, path
}

// insertLegacyAudit writes a pre-schema_version audit row: ten columns, no
// version marker, exactly what an older binary left behind.
func insertLegacyAudit(t *testing.T, db *sql.DB, invocationID, detail string) {
	t.Helper()
	execFixture(t, db, `
		INSERT INTO audit_log
			(ts, actor, approved_by, rationale, context_summary, session_id, invocation_id, action, target, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"2026-07-29T08:00:00Z", "test", "engineer", "legacy approval", "legacy context",
		"legacy-session", invocationID, actionRestartService, string(inventoryService), detail,
	)
}

// snapshot captures a file's bytes and modification time, so a test can prove a
// read-only path left the file untouched rather than merely left it valid.
type snapshot struct {
	modTime time.Time
	bytes   []byte
}

func takeSnapshot(t *testing.T, path string) snapshot {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return snapshot{bytes: content, modTime: info.ModTime()}
}

// assertUnchanged fails when the file differs from the snapshot in content or
// modification time.
func (s snapshot) assertUnchanged(t *testing.T, path string) {
	t.Helper()
	current := takeSnapshot(t, path)
	if string(current.bytes) != string(s.bytes) {
		t.Errorf("%s changed on disk: %d bytes before, %d after", path, len(s.bytes), len(current.bytes))
	}
	if !current.modTime.Equal(s.modTime) {
		t.Errorf("%s was rewritten: mtime %s before, %s after", path, s.modTime, current.modTime)
	}
}

// scalar reads a single value out of a fixture database.
func scalar[T any](t *testing.T, db *sql.DB, query string, args ...any) (T, bool) {
	t.Helper()
	var value T
	err := db.QueryRowContext(t.Context(), query, args...).Scan(&value)
	if err == nil {
		return value, true
	}
	if errors.Is(err, sql.ErrNoRows) {
		return value, false
	}
	t.Fatalf("fixture query %q: %v", query, err)
	return value, false
}

// countRows is the common shape of the assertions that check how many audit
// rows one logical write produced.
func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	count, found := scalar[int](t, db, query, args...)
	if !found {
		t.Fatalf("fixture count %q returned no row", query)
	}
	return count
}

// indexKeyColumns reads the keyed columns of the idempotency index straight out
// of a fixture connection, so the assertion does not go through the same guard
// it is verifying.
func indexKeyColumns(t *testing.T, db *sql.DB) []indexColumn {
	t.Helper()
	rows, err := db.QueryContext(t.Context(),
		`SELECT seqno, name, "desc", coll FROM pragma_index_xinfo(?) WHERE key = 1 ORDER BY seqno`,
		auditIdempotencyIndex,
	)
	if err != nil {
		t.Fatalf("read the index key columns: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var key []indexColumn
	for rows.Next() {
		var column indexColumn
		if err := rows.Scan(&column.Seqno, &column.Name, &column.Desc, &column.Coll); err != nil {
			t.Fatalf("scan the index key columns: %v", err)
		}
		key = append(key, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the index key columns: %v", err)
	}
	return key
}

// triggerNames lists the append-only triggers currently defined.
func triggerNames(t *testing.T, db *sql.DB) map[string]struct{} {
	t.Helper()
	rows, err := db.QueryContext(t.Context(),
		`SELECT name FROM sqlite_schema WHERE type = 'trigger' AND name LIKE 'audit_log_no_%'`)
	if err != nil {
		t.Fatalf("list the append-only triggers: %v", err)
	}
	defer func() { _ = rows.Close() }()
	names := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan the append-only triggers: %v", err)
		}
		names[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the append-only triggers: %v", err)
	}
	return names
}
