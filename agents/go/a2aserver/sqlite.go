package a2aserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	// The pure-Go SQLite driver (D-4). github.com/glebarez/sqlite — the GORM
	// dialector ADK's session store uses — is a thin layer over this package, so
	// importing it here keeps exactly one SQLite implementation in the binary
	// and no cgo.
	_ "github.com/glebarez/go-sqlite"
)

// sqliteDriver is the database/sql name github.com/glebarez/go-sqlite registers.
const sqliteDriver = "sqlite"

// Connection parameters, carried in the DSN rather than issued as PRAGMA
// statements, because the driver applies them while opening every connection it
// makes. A PRAGMA statement only configures whichever connection happened to
// run it, which a pooled handle is free to change underneath you.
var (
	// writeParameters make BEGIN take the write lock immediately.
	// _txlock=immediate is what turns database/sql's BeginTx into
	// `BEGIN IMMEDIATE`; without it BeginTx emits a deferred BEGIN that upgrades
	// to a write lock only at the first mutation, which would reopen the
	// read-then-write race that the task store's optimistic concurrency check
	// exists to close.
	writeParameters = []string{"_txlock=immediate", "_pragma=busy_timeout(5000)"}

	// readParameters open a database read-only twice over: mode=ro asks SQLite
	// for a read-only handle, and query_only refuses a write even if some future
	// caller obtains a writable one. Both preflight and readiness must be
	// provably incapable of changing the state they inspect.
	readParameters = []string{"mode=ro", "_pragma=query_only(1)", "_pragma=busy_timeout(5000)"}
)

// openSQLite opens path with the parameter set its mode requires and a pool
// capped at one connection.
//
// The single connection is the Python track's design decision carried over
// (pool_size=1, max_overflow=0): SQLite admits one writer, and one pooled
// connection queues short transactions instead of letting two handles race into
// intermittent "database is locked". It also makes a transaction's read and its
// write provably land on the same connection.
func openSQLite(ctx context.Context, path string, readOnly bool) (*sql.DB, error) {
	parameters := writeParameters
	if readOnly {
		parameters = readParameters
	}
	dsn, err := dataSourceName(path, parameters...)
	if err != nil {
		return nil, err
	}
	pool, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	pool.SetMaxOpenConns(1)
	// sql.Open is lazy, so nothing has touched the file yet. Connect eagerly so
	// "this path is not openable" surfaces here, where the caller can describe
	// it, rather than inside an unrelated query later.
	connection, err := pool.Conn(ctx)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("connect to %s: %w", filepath.Base(path), err), pool.Close())
	}
	// Hand the connection straight back: the pool is capped at one, so every
	// later statement lands on this same connection anyway, and holding a
	// *sql.Conn open alongside would deadlock the next query against the cap.
	if err := connection.Close(); err != nil {
		return nil, errors.Join(
			fmt.Errorf("release the probe connection for %s: %w", filepath.Base(path), err),
			pool.Close(),
		)
	}
	return pool, nil
}

// dataSourceName builds a SQLite URI DSN.
//
// The path travels as a file:// URI so a directory containing '?' or '#' cannot
// be mistaken for query parameters — the driver hands a `file:` DSN to SQLite
// whole, and SQLite is the one that parses it.
func dataSourceName(path string, parameters ...string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve the database path %s: %w", path, err)
	}
	uri := url.URL{Scheme: "file", Path: absolute}
	return uri.String() + "?" + strings.Join(parameters, "&"), nil
}

// closeWith joins a close failure into the error already being returned, so a
// failed close is never the reason a real failure disappears.
func closeWith(pool *sql.DB, name string, err error) error {
	closeErr := pool.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("close %s: %w", name, closeErr)
	}
	return errors.Join(err, closeErr)
}

// inWriteTransaction runs body inside one BEGIN IMMEDIATE, committing when body
// returns nil and rolling back otherwise.
//
// A named return plus a deferred rollback is the only shape that survives a
// panic in body: without it a panicking callback would leave the write lock
// held for the life of the process.
func inWriteTransaction(ctx context.Context, pool *sql.DB, body func(tx *sql.Tx) error) (err error) {
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin the transaction: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("roll back the transaction: %w", rollbackErr))
		}
	}()
	if err := body(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit the transaction: %w", err)
	}
	committed = true
	return nil
}

// quickCheck runs SQLite's cheap integrity check over an open handle.
//
// PRAGMA quick_check(1), not PRAGMA integrity_check: this is the shape the
// Python server used for its startup and readiness gates, and it is a
// deliberately different cost/strictness profile from the full check the state
// snapshot tooling runs. Do not unify the two.
func quickCheck(ctx context.Context, pool *sql.DB, name string) error {
	var status string
	switch err := pool.QueryRowContext(ctx, "PRAGMA quick_check(1)").Scan(&status); {
	case errors.Is(err, sql.ErrNoRows):
		status = "no result"
	case err != nil:
		return fmt.Errorf("run the integrity check on %s: %w", name, err)
	}
	if status != "ok" {
		return fmt.Errorf("%s integrity check failed: %s", name, status)
	}
	return nil
}

// sqliteSessionSchema reads a session store held in one SQLite file. It is the
// [sessionSchema] the file backend's preflight and readiness checks judge, and
// it adds nothing to the two queries below beyond binding them to one handle.
type sqliteSessionSchema struct{ queryer rowQueryer }

func (s sqliteSessionSchema) tables(ctx context.Context) (map[string]struct{}, error) {
	return presentTables(ctx, s.queryer)
}

func (s sqliteSessionSchema) columns(ctx context.Context, table string) (map[string]struct{}, error) {
	return tableColumns(ctx, s.queryer, table)
}

// presentTables returns the user tables a database declares, excluding SQLite's
// own internal ones.
func presentTables(ctx context.Context, queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
},
) (map[string]struct{}, error) {
	rows, err := queryer.QueryContext(ctx,
		`SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, fmt.Errorf("list the tables: %w", err)
	}
	defer func() { _ = rows.Close() }()
	tables := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan the table list: %w", err)
		}
		tables[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the table list: %w", err)
	}
	return tables, nil
}

// tableColumns returns the column names of one table, empty when the table does
// not exist.
func tableColumns(ctx context.Context, queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, table string,
) (map[string]struct{}, error) {
	// pragma_table_info takes the table as a bound parameter, so a table name is
	// never interpolated into SQL text.
	rows, err := queryer.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("inspect the columns of %q: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	columns := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan the columns of %q: %w", table, err)
		}
		columns[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the columns of %q: %w", table, err)
	}
	return columns, nil
}
