package a2aserver

import (
	"context"
	"database/sql"
	"fmt"
)

// The catalog queries behind the shared session store's checks.
//
// They read information_schema rather than pg_class on purpose: those views
// only show a table or a column the connected role holds some privilege on, so
// a session schema this deployment's role cannot touch reports as missing
// rather than as present-and-unusable.
//
// Both are scoped to current_schemas(false) — the connection's own search path
// minus the implicit pg_catalog — because that is exactly how the unqualified
// names in ADK's statements resolve. A deployment that puts the sessions in a
// dedicated schema and says so in its DSN is therefore checked where its data
// actually is, and a table of the same name in a schema outside the path is
// correctly invisible.
const (
	postgresSessionTablesQuery = `SELECT table_name FROM information_schema.tables
WHERE table_schema = ANY(current_schemas(false)) AND table_type = 'BASE TABLE'`
	postgresSessionColumnsQuery = `SELECT column_name FROM information_schema.columns
WHERE table_schema = ANY(current_schemas(false)) AND table_name = $1`
)

// postgresSessionSchema reads a session store held on a PostgreSQL server
// through one pool. It is the [sessionSchema] the shared backend is judged by.
type postgresSessionSchema struct{ queryer rowQueryer }

func (p postgresSessionSchema) tables(ctx context.Context) (map[string]struct{}, error) {
	names, err := scanNames(ctx, p.queryer, postgresSessionTablesQuery)
	if err != nil {
		return nil, fmt.Errorf("list the PostgreSQL session tables: %w", err)
	}
	return names, nil
}

func (p postgresSessionSchema) columns(ctx context.Context, table string) (map[string]struct{}, error) {
	// The table travels as a bound parameter, so a name is never interpolated
	// into SQL text — the same rule the SQLite reader follows.
	names, err := scanNames(ctx, p.queryer, postgresSessionColumnsQuery, table)
	if err != nil {
		return nil, fmt.Errorf("inspect the PostgreSQL columns of %q: %w", table, err)
	}
	return names, nil
}

// scanNames runs a query returning one name per row and collects them into a
// set.
func scanNames(
	ctx context.Context, queryer rowQueryer, query string, arguments ...any,
) (map[string]struct{}, error) {
	rows, err := queryer.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	names := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return names, nil
}

// ProbePostgresSessionStore reports whether the shared session store is
// reachable and carries the schema ADK needs. It is the readiness check a
// deployment running config.SessionBackendPostgres supplies as
// [Config.ProbeSessions].
//
// # It asks readiness' own question, in the shared backend's terms
//
// [probeStateStores] asks whether the state on disk is usable. With the
// sessions on a server, the equivalent question is whether the schema every
// replica writes through is reachable and well formed, so that is what this
// asks: it resolves ADK's four tables through the connection's search path and
// compares their columns against [sessionStoreColumns] — the same list the file
// backend is judged by, because gorm names them the same way on both engines. A
// database that is unreachable, that no migration has run against, or that a
// generation with a different shape created is reported here, exactly as an
// absent or half-built runtime.db is on the file backend.
//
// # Why this one borrows the pool the turns use
//
// The file probes open their own read-only handle because a SQLite store holds
// a single connection by design, so one live turn would otherwise queue the
// probe behind it and readiness would report load as failure. A PostgreSQL pool
// is bounded but plural, and its ceiling is shared: replicas multiplied by
// AGENT_SESSION_MAX_CONNS must stay under the server's max_connections, so a
// probe connection opened outside that pool would spend a connection outside
// that budget on every poll — and a load-shedding server would then refuse the
// probe of a replica that was serving perfectly well. Borrowing also makes the
// answer stronger: it proves the exact pool, credentials and server the turns
// use are working, not that some other connection could have been opened. If
// every pooled connection is busy for the probe's whole deadline, this replica
// genuinely cannot start a turn, and reporting unready is then the truth rather
// than a self-inflicted wound.
//
// Nothing here writes: two catalog reads, no transaction, no schema change. The
// migration belongs to startup, which is the single moment this application
// changes a session schema.
func ProbePostgresSessionStore(ctx context.Context, pool *sql.DB) error {
	schema := postgresSessionSchema{queryer: pool}
	tables, err := schema.tables(ctx)
	if err != nil {
		return err
	}
	return checkSessionSchema(ctx, schema, tables, sharedSessionStore)
}
