package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	// The pure-Go SQLite driver. github.com/glebarez/sqlite — the driver ADK's
	// session store uses — is a GORM dialector over this package, so importing it
	// here keeps exactly one SQLite implementation in the binary and no cgo.
	_ "github.com/glebarez/go-sqlite"
)

// sqliteDriver is the database/sql name github.com/glebarez/go-sqlite registers.
const sqliteDriver = "sqlite"

// stateDirPerm is the mode of the runtime state directory. It matches the data
// package, so the two stores that share the directory agree about it.
const stateDirPerm = 0o750

// The two databases this package owns, both inside AGENT_STATE_DIR. Neither
// ever lives in the committed dataset: `mise run data:reset` deletes the
// directory, and everything here has to be rebuildable from nothing.
const (
	notesDatabaseName   = "memory.db"
	vectorsDatabaseName = "vectors.db"
)

// stateDatabasePath returns the path of one database inside the disposable
// state directory, creating the directory if it does not exist.
//
// Creating it here is deliberate and matches the Python original: the note
// store and the vector index are the first things to touch the directory on a
// fresh checkout, and both would otherwise fail on a missing parent rather than
// on anything to do with memory.
func stateDatabasePath(stateDir, name string) (string, error) {
	if err := os.MkdirAll(stateDir, stateDirPerm); err != nil {
		return "", fmt.Errorf("create the runtime state directory %s: %w", stateDir, err)
	}
	return filepath.Join(stateDir, name), nil
}

// openStateDatabase opens one of this package's SQLite databases.
//
// Connection parameters travel in the DSN rather than as PRAGMA statements: the
// driver applies them while opening every connection it makes, so a pooled
// connection can never miss one. The pool is capped at a single connection
// because SQLite transaction state is per connection and database/sql is
// otherwise free to run the next statement somewhere else.
//
// immediate turns database/sql's BeginTx into `BEGIN IMMEDIATE`, which takes
// the write lock up front. That is what serializes two processes racing to
// build the same vector generation; a deferred BEGIN would upgrade to a write
// lock only at the first mutation, after both had already decided to rebuild.
func openStateDatabase(ctx context.Context, path string, busyTimeout time.Duration, immediate bool) (*sql.DB, error) {
	parameters := []string{"_pragma=busy_timeout(" + strconv.FormatInt(busyTimeout.Milliseconds(), 10) + ")"}
	if immediate {
		parameters = append(parameters, "_txlock=immediate")
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
	// sql.Open is lazy, so nothing has touched the file yet. Connect eagerly:
	// "this path is not openable" must surface here, where the caller can give it
	// the right message, rather than inside an unrelated query later.
	if err := pool.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("connect to %s: %w", filepath.Base(path), err), pool.Close())
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
func closeWith(db *sql.DB, err error) error {
	closeErr := db.Close()
	if closeErr != nil {
		closeErr = fmt.Errorf("close the database: %w", closeErr)
	}
	return errors.Join(err, closeErr)
}

// rollback abandons a transaction, joining any rollback failure into the error
// that caused the abandonment.
//
// A rollback that fails after the connection is already broken carries no
// information the original failure did not, but it must not be swallowed
// either: a rollback that fails while the connection is healthy means the
// transaction may still be open.
func rollback(tx *sql.Tx, err error) error {
	rollbackErr := tx.Rollback()
	if rollbackErr == nil || errors.Is(rollbackErr, sql.ErrTxDone) {
		return err
	}
	return errors.Join(err, fmt.Errorf("roll back the transaction: %w", rollbackErr))
}
