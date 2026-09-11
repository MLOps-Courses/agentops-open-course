package a2aserver

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
)

// TestRequiredSessionColumnsMatchADK is the ratchet behind [sessionStoreColumns].
//
// The Python server checked ADK Python's SQLAlchemy schema, which Go does not
// have; this list was read off ADK Go's gorm storage models instead. A list
// read off a dependency rots the moment that dependency renames a field, and
// the failure mode is the worst kind: a server that refuses to start against a
// database it created itself. So the list is compared against a freshly
// migrated database on every run, and an upstream rename fails here — in a
// test that names the missing column — rather than in production.
func TestRequiredSessionColumnsMatchADK(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), SessionDatabaseName)
	sessions, openErr := database.NewSessionService(sqlite.Open("file:" + path))
	if openErr != nil {
		t.Fatalf("opening the session store: %v", openErr)
	}
	if err := database.AutoMigrate(sessions); err != nil {
		t.Fatalf("migrating the session store: %v", err)
	}

	pool, poolErr := openSQLite(t.Context(), path, true)
	if poolErr != nil {
		t.Fatalf("reopening the session store read-only: %v", poolErr)
	}
	defer func() {
		if err := pool.Close(); err != nil {
			t.Errorf("closing the session store: %v", err)
		}
	}()

	tables, tablesErr := presentTables(t.Context(), pool)
	if tablesErr != nil {
		t.Fatalf("listing the migrated tables: %v", tablesErr)
	}
	for _, table := range sortedTables(sessionStoreColumns) {
		if _, ok := tables[table]; !ok {
			t.Errorf("ADK no longer creates table %q", table)
			continue
		}
		columns, err := tableColumns(t.Context(), pool, table)
		if err != nil {
			t.Fatalf("inspecting %q: %v", table, err)
		}
		for _, column := range sessionStoreColumns[table] {
			if _, ok := columns[column]; !ok {
				t.Errorf("ADK no longer creates column %q on table %q", column, table)
			}
		}
	}

	// The schema check must also accept the database ADK just created, which is
	// the property a column list read off a dependency can lose without any
	// individual column being wrong.
	if err := checkSessionSchema(t.Context(), sqliteSessionSchema{queryer: pool}, tables, fileSessionStore); err != nil {
		t.Errorf("checkSessionSchema() rejected a freshly migrated database: %v", err)
	}
}

// TestTaskStoreSchemaIsSelfConsistent proves the DDL and the required-shape
// list describe the same table. They are separate declarations because one is
// what a fresh store is created with and the other is what an existing store is
// judged against, and a store that could not pass its own check would refuse to
// start on its second boot.
func TestTaskStoreSchemaIsSelfConsistent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), TaskDatabaseName)
	store, openErr := OpenTaskStore(t.Context(), TaskStoreConfig{Path: path})
	if openErr != nil {
		t.Fatalf("OpenTaskStore() error = %v, want nil", openErr)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}

	pool, poolErr := openSQLite(t.Context(), path, true)
	if poolErr != nil {
		t.Fatalf("reopening the task store read-only: %v", poolErr)
	}
	defer func() {
		if err := pool.Close(); err != nil {
			t.Errorf("closing the task store: %v", err)
		}
	}()

	tables, tablesErr := presentTables(t.Context(), pool)
	if tablesErr != nil {
		t.Fatalf("listing the created tables: %v", tablesErr)
	}
	for _, table := range taskStoreTables {
		if _, ok := tables[table]; !ok {
			t.Errorf("OpenTaskStore did not create table %q", table)
		}
	}
	columns, columnsErr := tableColumns(t.Context(), pool, tasksTable)
	if columnsErr != nil {
		t.Fatalf("inspecting %q: %v", tasksTable, columnsErr)
	}
	for _, column := range taskStoreColumns {
		if _, ok := columns[column]; !ok {
			t.Errorf("OpenTaskStore did not create column %q", column)
		}
	}
	if err := checkTaskStoreVersion(t.Context(), pool, tables); err != nil {
		t.Errorf("checkTaskStoreVersion() rejected a freshly created store: %v", err)
	}
}

// TestReopeningAnExistingTaskStoreIsIdempotent covers the second boot: opening
// a prepared store must neither refuse it nor rewrite it.
func TestReopeningAnExistingTaskStoreIsIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), TaskDatabaseName)
	for attempt := range 3 {
		store, err := OpenTaskStore(t.Context(), TaskStoreConfig{Path: path})
		if err != nil {
			t.Fatalf("OpenTaskStore() attempt %d error = %v, want nil", attempt, err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("Close() attempt %d error = %v, want nil", attempt, err)
		}
	}

	pool, openErr := sql.Open(sqliteDriver, "file:"+path)
	if openErr != nil {
		t.Fatalf("opening the task store: %v", openErr)
	}
	defer func() {
		if err := pool.Close(); err != nil {
			t.Errorf("closing the task store: %v", err)
		}
	}()
	rows, rowsErr := pool.QueryContext(t.Context(), `SELECT key FROM store_metadata`)
	if rowsErr != nil {
		t.Fatalf("reading the marker table: %v", rowsErr)
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scanning the marker table: %v", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the marker table: %v", err)
	}
	if want := []string{schemaVersionKey}; !slices.Equal(keys, want) {
		t.Errorf("marker keys = %v, want %v", keys, want)
	}
}

// TestConcurrentSessionWritesQueueUnderTheDocumentedDSN is the regression test
// behind [SessionDataSourceName].
//
// Opened without _txlock=immediate, ADK's session service loses a concurrent
// first use to SQLITE_BUSY: two turns both take a read lock inside a deferred
// transaction and neither can upgrade. That is a lost turn in production, not a
// slow one, so the DSN that prevents it is pinned here rather than left to a
// comment.
func TestConcurrentSessionWritesQueueUnderTheDocumentedDSN(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	sessions, openErr := database.NewSessionService(sqlite.Open(SessionDataSourceName(stateDir)))
	if openErr != nil {
		t.Fatalf("opening the session store: %v", openErr)
	}
	if err := database.AutoMigrate(sessions); err != nil {
		t.Fatalf("migrating the session store: %v", err)
	}

	const writers = 8
	var (
		wait     sync.WaitGroup
		guard    sync.Mutex
		failures []error
	)
	for index := range writers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := sessions.Create(t.Context(), &session.CreateRequest{
				AppName:   "app",
				UserID:    "user-" + strconv.Itoa(index),
				SessionID: "session-" + strconv.Itoa(index),
				State:     map[string]any{},
			})
			if err != nil {
				guard.Lock()
				defer guard.Unlock()
				failures = append(failures, err)
			}
		}()
	}
	wait.Wait()

	if len(failures) > 0 {
		t.Errorf("%d of %d concurrent session creates failed: %v", len(failures), writers, failures)
	}
}
