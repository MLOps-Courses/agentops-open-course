package a2aserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	// The pure-Go PostgreSQL driver, registered as "pgx". Only the integration
	// test below dials with it; every other test here stays offline.
	_ "github.com/jackc/pgx/v5/stdlib"
	"google.golang.org/adk/v2/session/database"
	gormpostgres "gorm.io/driver/postgres"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
)

// envIntegrationDSN names a throwaway PostgreSQL database for the one test that
// needs a live server. It is deliberately not AGENT_SESSION_DSN: that variable
// points at a deployment's real sessions, and this test creates and drops a
// schema.
const envIntegrationDSN = "AGENT_TEST_SESSION_DSN"

// stubSessionSchema answers the two catalog questions from a fixed description
// of a store, so the report a broken schema produces can be asserted without an
// engine behind it.
type stubSessionSchema struct {
	columnsOf map[string][]string
	failure   error
}

func (s stubSessionSchema) tables(context.Context) (map[string]struct{}, error) {
	names := make(map[string]struct{}, len(s.columnsOf))
	for table := range s.columnsOf {
		names[table] = struct{}{}
	}
	return names, nil
}

func (s stubSessionSchema) columns(_ context.Context, table string) (map[string]struct{}, error) {
	if s.failure != nil {
		return nil, s.failure
	}
	names := make(map[string]struct{}, len(s.columnsOf[table]))
	for _, column := range s.columnsOf[table] {
		names[column] = struct{}{}
	}
	return names, nil
}

// migratedSessionSchema describes a store this application has just migrated:
// every required table, every required column.
func migratedSessionSchema() map[string][]string {
	described := make(map[string][]string, len(sessionStoreColumns))
	for table, columns := range sessionStoreColumns {
		described[table] = append([]string(nil), columns...)
	}
	return described
}

// TestSessionSchemaReportNamesTheStoreItJudged pins the operator-facing half of
// the two-backend check.
//
// One judgement serves both stores, so the report has to say which one it is
// about and what can be done to it. A file in AGENT_STATE_DIR is replaced by
// selecting a snapshot; a shared server has no snapshot, and naming one would
// send an operator to a directory that holds nothing relevant. Neither message
// may carry the DSN, which is a credential.
func TestSessionSchemaReportNamesTheStoreItJudged(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		damage   func(map[string][]string)
		target   sessionStoreTarget
		wants    []string
		unwanted []string
	}{
		"the file store is missing a table": {
			target: fileSessionStore,
			damage: func(schema map[string][]string) { delete(schema, "events") },
			wants: []string{
				SessionDatabaseName, "incomplete current schema",
				"missing tables: events", selectSnapshot,
			},
		},
		"the file store is missing a column": {
			target: fileSessionStore,
			damage: func(schema map[string][]string) { schema["events"] = []string{"id"} },
			wants: []string{
				SessionDatabaseName, `table "events" is missing columns`, "content", selectSnapshot,
			},
		},
		"the shared store is missing a table": {
			target: sharedSessionStore,
			damage: func(schema map[string][]string) { delete(schema, "events") },
			wants: []string{
				"PostgreSQL", config.EnvSessionDSN, "missing tables: events", migrateSharedStore,
			},
			// A snapshot of AGENT_STATE_DIR cannot repair a shared server, and
			// runtime.db is not where these sessions are.
			unwanted: []string{SessionDatabaseName, "snapshot"},
		},
		"the shared store is missing a column": {
			target: sharedSessionStore,
			damage: func(schema map[string][]string) { schema["sessions"] = []string{"id"} },
			wants: []string{
				"PostgreSQL", config.EnvSessionDSN, `table "sessions" is missing columns`,
				"app_name", migrateSharedStore,
			},
			unwanted: []string{SessionDatabaseName, "snapshot"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			described := migratedSessionSchema()
			testCase.damage(described)
			schema := stubSessionSchema{columnsOf: described}
			tables, err := schema.tables(t.Context())
			if err != nil {
				t.Fatalf("tables() error = %v, want nil", err)
			}

			err = checkSessionSchema(t.Context(), schema, tables, testCase.target)
			if err == nil {
				t.Fatal("checkSessionSchema() error = nil, want a report")
			}
			for _, want := range testCase.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("report does not contain %q\nfull report:\n%s", want, err)
				}
			}
			for _, unwanted := range testCase.unwanted {
				if strings.Contains(err.Error(), unwanted) {
					t.Errorf("report contains %q, which belongs to the other backend\nfull report:\n%s", unwanted, err)
				}
			}
		})
	}
}

// TestSessionSchemaAcceptsAMigratedStoreOnEitherBackend is the other half: the
// same required shape must pass on both, because gorm creates the same tables
// and columns whichever dialector it was given.
func TestSessionSchemaAcceptsAMigratedStoreOnEitherBackend(t *testing.T) {
	t.Parallel()

	schema := stubSessionSchema{columnsOf: migratedSessionSchema()}
	tables, err := schema.tables(t.Context())
	if err != nil {
		t.Fatalf("tables() error = %v, want nil", err)
	}
	for _, target := range []sessionStoreTarget{fileSessionStore, sharedSessionStore} {
		if err := checkSessionSchema(t.Context(), schema, tables, target); err != nil {
			t.Errorf("checkSessionSchema(%s) rejected a migrated store: %v", target.name, err)
		}
	}
}

// TestSessionSchemaSurfacesACatalogFailure proves a store that cannot be read
// is reported rather than treated as complete. A swallowed catalog error would
// be a readiness check that passes because it learned nothing.
func TestSessionSchemaSurfacesACatalogFailure(t *testing.T) {
	t.Parallel()

	unreadable := errors.New("permission denied for schema public")
	schema := stubSessionSchema{columnsOf: migratedSessionSchema(), failure: unreadable}
	tables, err := schema.tables(t.Context())
	if err != nil {
		t.Fatalf("tables() error = %v, want nil", err)
	}
	if err := checkSessionSchema(t.Context(), schema, tables, sharedSessionStore); !errors.Is(err, unreadable) {
		t.Errorf("checkSessionSchema() error = %v, want the catalog failure", err)
	}
}

// TestPostgresSessionProbeReportsAnUnusablePool covers the failure an operator
// meets first: the server is unreachable, the credentials are refused, or the
// pool has been closed by shutdown. Readiness must report that, and must not
// mistake "could not ask" for "nothing is missing".
func TestPostgresSessionProbeReportsAnUnusablePool(t *testing.T) {
	t.Parallel()

	// A closed pool stands in for every way a connection can be unavailable, and
	// keeps this test offline. The driver is irrelevant: the queries never run.
	pool, err := sql.Open(sqliteDriver, "file:"+filepath.Join(t.TempDir(), "unused.db"))
	if err != nil {
		t.Fatalf("opening the stand-in pool: %v", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("closing the stand-in pool: %v", err)
	}

	probeErr := ProbePostgresSessionStore(t.Context(), pool)
	if probeErr == nil {
		t.Fatal("ProbePostgresSessionStore() error = nil against a closed pool")
	}
	if !strings.Contains(probeErr.Error(), "list the PostgreSQL session tables") {
		t.Errorf("error = %v, want it to name the failed catalog read", probeErr)
	}

	_, columnsErr := postgresSessionSchema{queryer: pool}.columns(t.Context(), "events")
	if columnsErr == nil {
		t.Fatal("columns() error = nil against a closed pool")
	}
	if !strings.Contains(columnsErr.Error(), `columns of "events"`) {
		t.Errorf("error = %v, want it to name the table it failed to inspect", columnsErr)
	}
}

// TestPostgresSessionProbeAgainstARealServer is the shared backend's ratchet,
// and the only test in this module that needs a database server.
//
// Everything above judges the report; this judges the two catalog queries and
// the claim underneath them — that a database ADK migrated on PostgreSQL
// carries the very tables and columns [sessionStoreColumns] lists. That claim
// cannot be checked against a fake, so it is checked against a server when one
// is offered and reported as unproven when it is not. It never passes quietly:
// a DSN that is set but unreachable fails the test rather than skipping it.
func TestPostgresSessionProbeAgainstARealServer(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv(envIntegrationDSN))
	if dsn == "" {
		t.Skipf(
			"%[1]s is unset, so the shared session-store probe was NOT verified against a real server.\n"+
				"  docker run --rm -d --name agentops-sessions -p 55432:5432 \\\n"+
				"    -e POSTGRES_USER=agentops -e POSTGRES_PASSWORD=<password> -e POSTGRES_DB=sessions \\\n"+
				"    postgres:18.3-alpine\n"+
				"  %[1]s=postgres://agentops:<password>@127.0.0.1:55432/sessions?sslmode=disable go test ./a2aserver/",
			envIntegrationDSN,
		)
	}

	// One schema per run, so the test owns every table it judges and leaves the
	// database as it found it.
	schemaName := fmt.Sprintf("agentops_probe_%d", time.Now().UnixNano())
	admin := openIntegrationPool(t, dsn)
	if err := admin.PingContext(t.Context()); err != nil {
		t.Fatalf("%s is set but the server is unreachable: %v", envIntegrationDSN, err)
	}
	if _, err := admin.ExecContext(t.Context(), `CREATE SCHEMA `+quoteIdentifier(schemaName)); err != nil {
		t.Fatalf("creating the test schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(
			context.Background(), `DROP SCHEMA `+quoteIdentifier(schemaName)+` CASCADE`,
		); err != nil {
			t.Errorf("dropping the test schema: %v", err)
		}
	})

	pool := openIntegrationPool(t, searchPathDSN(t, dsn, schemaName))
	// Without this, a search path that silently failed to apply would leave the
	// whole test judging the public schema and passing for the wrong reason.
	var current string
	if err := pool.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&current); err != nil {
		t.Fatalf("reading the connection's current schema: %v", err)
	}
	if current != schemaName {
		t.Fatalf("current_schema() = %q, want %q; the search path did not reach the driver", current, schemaName)
	}

	// Before the migration the store is empty, which after startup is a failure
	// exactly as an empty runtime.db is.
	err := ProbePostgresSessionStore(t.Context(), pool)
	if err == nil {
		t.Fatal("ProbePostgresSessionStore() error = nil against an unmigrated database")
	}
	for _, want := range []string{"missing tables", config.EnvSessionDSN} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("report does not contain %q\nfull report:\n%s", want, err)
		}
	}

	sessions, openErr := database.NewSessionService(gormpostgres.New(gormpostgres.Config{Conn: pool}))
	if openErr != nil {
		t.Fatalf("opening the ADK session store: %v", openErr)
	}
	if migrateErr := database.AutoMigrate(sessions); migrateErr != nil {
		t.Fatalf("migrating the ADK session store: %v", migrateErr)
	}
	if probeErr := ProbePostgresSessionStore(t.Context(), pool); probeErr != nil {
		t.Fatalf("ProbePostgresSessionStore() rejected a freshly migrated PostgreSQL store: %v", probeErr)
	}

	// A column ADK relies on, removed the way an unrelated migration would.
	if _, alterErr := pool.ExecContext(t.Context(), `ALTER TABLE events DROP COLUMN content`); alterErr != nil {
		t.Fatalf("dropping the event payload column: %v", alterErr)
	}
	err = ProbePostgresSessionStore(t.Context(), pool)
	if err == nil {
		t.Fatal("ProbePostgresSessionStore() error = nil with the event payload column gone")
	}
	for _, want := range []string{`table "events" is missing columns content`, migrateSharedStore} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("report does not contain %q\nfull report:\n%s", want, err)
		}
	}

	if _, dropErr := pool.ExecContext(t.Context(), `DROP TABLE events`); dropErr != nil {
		t.Fatalf("dropping the events table: %v", dropErr)
	}
	err = ProbePostgresSessionStore(t.Context(), pool)
	if err == nil {
		t.Fatal("ProbePostgresSessionStore() error = nil with the events table gone")
	}
	if !strings.Contains(err.Error(), "missing tables: events") {
		t.Errorf("report does not name the dropped table\nfull report:\n%s", err)
	}
}

// openIntegrationPool opens one bounded pool on the integration server and
// closes it when the test ends.
func openIntegrationPool(t *testing.T, dsn string) *sql.DB {
	t.Helper()

	pool, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("opening the integration pool: %v", err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("closing the integration pool: %v", err)
		}
	})
	return pool
}

// searchPathDSN points a DSN at one schema, which is how this test proves the
// probe follows the connection's search path rather than assuming "public".
func searchPathDSN(t *testing.T, dsn, schemaName string) string {
	t.Helper()

	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("%s must be a postgres:// URL: %v", envIntegrationDSN, err)
	}
	query := parsed.Query()
	query.Set("search_path", schemaName)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// quoteIdentifier renders a generated schema name as a SQL identifier. The name
// is built from a timestamp here, so this is belt and braces rather than a
// defense against input.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
