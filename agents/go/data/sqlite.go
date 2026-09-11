package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	// The pure-Go SQLite driver. github.com/glebarez/sqlite — the driver ADK's
	// session store uses — is a GORM dialector over this package, so importing
	// it here keeps exactly one SQLite implementation in the binary and no cgo.
	_ "github.com/glebarez/go-sqlite"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/safefile"
)

// sqliteDriver is the database/sql name github.com/glebarez/go-sqlite registers.
const sqliteDriver = "sqlite"

// Connection parameters, carried in the DSN rather than issued as PRAGMA
// statements. The driver applies them while opening *every* connection it
// makes, so a pooled connection can never miss one — a PRAGMA statement only
// configures whichever connection happened to run it.
var (
	// readParameters open the database read-only twice over: mode=ro asks
	// SQLite for a read-only handle, and query_only refuses writes even if some
	// future caller manages to obtain a writable one. The probe and every read
	// tool must be provably incapable of touching the committed seed.
	readParameters = []string{"mode=ro", "_pragma=query_only(1)", "_pragma=busy_timeout(5000)"}

	// writeParameters enforce foreign keys and make BEGIN take the write lock
	// immediately. _txlock=immediate is what turns database/sql's BeginTx into
	// `BEGIN IMMEDIATE` (verified in the driver's newTx); without it BeginTx
	// emits a deferred BEGIN, which would upgrade to a write lock only at the
	// first mutation and reopen the read-then-write race the audit trail exists
	// to close.
	writeParameters = []string{"mode=rw", "_txlock=immediate", "_pragma=foreign_keys(1)", "_pragma=busy_timeout(5000)"}
)

// querier is the read surface shared by *sql.DB and *sql.Tx.
//
// The schema guards take one so they can run either standalone (the read-only
// probe) or inside the migration transaction, which is exactly what the Python
// helpers got from being handed a single connection object.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// execer is the write surface [appendAudit] needs.
//
// It is an interface rather than *sql.Tx so a test can supply a result whose
// LastInsertId fails, which is the Go counterpart of Python's cursor.lastrowid
// being None. No real driver produces that on demand, and the branch that
// handles it is a genuine database boundary worth covering.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// database is one SQLite connection pool constrained to a single connection.
//
// The single connection is not a performance choice: SQLite transactions and
// connection PRAGMAs are per-connection state, and a database/sql pool is free
// to run the next statement somewhere else. One connection per operation is
// also the shape Python had, where every call opened and closed its own.
type database struct {
	*sql.DB
	reference *safefile.Regular

	// name is the base file name, which is all the Python error messages
	// carried — an absolute path in a tool result is an information leak the
	// model has no use for.
	name     string
	readOnly bool
}

// openDatabaseWithHook keeps a no-follow reference open across SQLite's lazy
// pathname resolution, then verifies that the pathname still names the same
// inode before any query or migration can run.
func openDatabaseWithHook(
	ctx context.Context,
	path string,
	readOnly bool,
	beforeConnect func(string),
) (*database, error) {
	reference, err := safefile.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s as a stable regular file: %w", filepath.Base(path), err)
	}
	fail := func(err error) (*database, error) {
		return nil, errors.Join(err, reference.Close())
	}
	if beforeConnect != nil {
		beforeConnect(path)
	}
	parameters := writeParameters
	if readOnly {
		parameters = readParameters
	}
	dsn, err := dataSourceName(path, parameters...)
	if err != nil {
		return fail(err)
	}
	pool, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		return fail(fmt.Errorf("open %s: %w", filepath.Base(path), err))
	}
	pool.SetMaxOpenConns(1)
	// sql.Open is lazy, so nothing has touched the file yet. Connect eagerly:
	// "this path is not openable" must surface here, where the caller can give
	// it the right message, rather than inside an unrelated query later.
	connection, err := pool.Conn(ctx)
	if err != nil {
		return fail(errors.Join(fmt.Errorf("connect to %s: %w", filepath.Base(path), err), pool.Close()))
	}
	// Hand the connection straight back: the pool is capped at one, so every
	// later statement lands on this same connection anyway, and holding a
	// *sql.Conn open alongside would deadlock the next query against the cap.
	if err := connection.Close(); err != nil {
		return fail(errors.Join(fmt.Errorf("release the probe connection for %s: %w", filepath.Base(path), err), pool.Close()))
	}
	if err := reference.Verify(); err != nil {
		return fail(errors.Join(fmt.Errorf("verify %s after SQLite connected: %w", filepath.Base(path), err), pool.Close()))
	}
	return &database{DB: pool, reference: reference, name: filepath.Base(path), readOnly: readOnly}, nil
}

// Close releases SQLite's pool and the no-follow reference guarding its path.
func (d *database) Close() error {
	return errors.Join(d.DB.Close(), d.reference.Close())
}

// dataSourceName builds a SQLite URI DSN.
//
// The path travels as a file:// URI so a directory containing '?' or '#'
// cannot be mistaken for query parameters — the driver hands a `file:` DSN to
// SQLite whole, and SQLite is the one that parses it.
func dataSourceName(path string, parameters ...string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		// Deliberately not an ErrDataAccess: the caller decides how to describe
		// a failure to open, and the probe in particular has its own wording.
		return "", fmt.Errorf("resolve the database path %s: %w", path, err)
	}
	uri := url.URL{Scheme: "file", Path: absolute}
	return uri.String() + "?" + strings.Join(parameters, "&"), nil
}

// fail gives a driver error the context Python's two connection managers gave
// it.
//
// A guard's own ErrDataAccess passes through untouched, which is what let the
// future-schema guidance reach the operator verbatim in Python: it raised
// DataAccessError, and the handler there only caught sqlite3.Error.
func (d *database) fail(err error) error {
	if err == nil || errors.Is(err, ErrDataAccess) {
		return err
	}
	operation := "SQLite operation failed"
	if d.readOnly {
		operation = "SQLite read failed"
	}
	return fmt.Errorf("%w: %s for %s: %w", ErrDataAccess, operation, d.name, err)
}

// closeWith joins a close failure into the error already being returned, so a
// failed close is never the reason a real failure disappears.
func (d *database) closeWith(err error) error {
	closeErr := d.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("%w: could not close %s: %w", ErrDataAccess, d.name, closeErr)
	}
	return errors.Join(err, closeErr)
}

// bind matches a SELECT *'s result columns to scan destinations by name.
//
// Python got this for free from sqlite3.Row plus dict(row): the row arrived
// keyed by column name, and pydantic's extra="forbid" rejected anything the
// model did not declare. database/sql only scans positionally, so binding by
// position would silently shift every field one place the day the table gains a
// column. Requiring an exact bijection reproduces both properties at once — an
// unexpected column and a missing one are equally a corrupt dataset.
func bind(columns []string, fields map[string]any) ([]any, error) {
	if len(columns) != len(fields) {
		return nil, fmt.Errorf("%w: expected %d columns, got %d: %s",
			ErrDataAccess, len(fields), len(columns), strings.Join(columns, ", "))
	}
	destinations := make([]any, len(columns))
	for position, name := range columns {
		field, ok := fields[name]
		if !ok {
			return nil, fmt.Errorf("%w: unexpected column %q", ErrDataAccess, name)
		}
		destinations[position] = field
	}
	// Equal lengths plus every column found means the match is a bijection:
	// SELECT * never repeats a name, so nothing can be missing.
	return destinations, nil
}

// utcNow renders the current time the way every timestamp in the dataset is
// written: ISO-8601 UTC at second resolution with a Z suffix.
func utcNow() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }

// readPath returns the database a read should open: published runtime state
// when it exists, otherwise the committed seed.
//
// This fallback is why the whole read surface works on a fresh checkout with
// nothing in the state directory, and why a read never publishes state —
// [Store.DBPath] is not on this path.
func (s *Store) readPath() (string, error) {
	if err := s.verifyStateDirectory(); err != nil {
		return "", err
	}
	runtimePath := s.RuntimePath()
	if err := verifyRegularPath(runtimePath); err == nil {
		return runtimePath, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%w: Runtime database must be a stable regular file: %s: %w",
			ErrDataAccess, filepath.Base(runtimePath), err)
	}
	seed := s.SeedPath()
	if err := verifyRegularPath(seed); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: Database is missing: %s", ErrDataAccess, filepath.Base(seed))
		}
		return "", fmt.Errorf("%w: Seed database must be a stable regular file: %s: %w",
			ErrDataAccess, filepath.Base(seed), err)
	}
	return seed, nil
}

// openForRead opens runtime state or the seed read-only, refusing at the door
// any database whose audit rows this binary cannot parse.
func (s *Store) openForRead(ctx context.Context) (*database, error) {
	path, err := s.readPath()
	if err != nil {
		return nil, err
	}
	db, err := openDatabaseWithHook(ctx, path, true, s.beforeDatabaseConnect)
	if err != nil {
		return nil, fmt.Errorf("%w: Could not open database read-only: %s: %w", ErrDataAccess, filepath.Base(path), err)
	}
	if err := rejectFutureAuditSchema(ctx, db); err != nil {
		return nil, db.closeWith(db.fail(err))
	}
	return db, nil
}

// openForWrite migrates runtime state, then opens the mutation connection.
//
// Every public write goes through here, so every write re-checks the audit
// schema before it touches a row. That is what makes an unsupported snapshot
// impossible to append to, not merely impossible to read.
func (s *Store) openForWrite(ctx context.Context) (*database, error) {
	path, err := s.PrepareRuntimeDatabase(ctx)
	if err != nil {
		return nil, err
	}
	db, err := openDatabaseWithHook(ctx, path, false, s.beforeDatabaseConnect)
	if err != nil {
		return nil, fmt.Errorf("%w: Could not open database: %s: %w", ErrDataAccess, filepath.Base(path), err)
	}
	return db, nil
}
