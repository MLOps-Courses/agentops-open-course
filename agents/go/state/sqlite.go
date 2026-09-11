package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	// The pure-Go SQLite driver. It is the same one the data package and ADK's
	// session store use, so the static binary carries exactly one SQLite
	// implementation and no cgo.
	_ "github.com/glebarez/go-sqlite"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// sqliteDriver is the database/sql name github.com/glebarez/go-sqlite registers.
const sqliteDriver = "sqlite"

// schemaIdentity is the fingerprint a snapshot records for each database, so a
// restore can prove the file it is about to publish still has the schema the
// manifest was signed over.
type schemaIdentity struct {
	// AuditVersion is the highest application audit schema version present, or
	// nil when the table has no schema_version column. It is `any` because
	// SQLite's MAX over a column with mixed storage classes can return text,
	// and the manifest has to record what was actually there.
	AuditVersion any
	// RuntimeVersion is the ADK session schema marker, or nil when the runtime
	// metadata table is absent.
	RuntimeVersion *string
	SchemaSHA256   string
	UserVersion    int64
}

// payload renders the identity as the manifest's `sqlite` object.
func (i schemaIdentity) payload() map[string]any {
	var runtime any
	if i.RuntimeVersion != nil {
		runtime = *i.RuntimeVersion
	}
	return map[string]any{
		"schema_sha256":          i.SchemaSHA256,
		"user_version":           i.UserVersion,
		"audit_schema_version":   i.AuditVersion,
		"runtime_schema_version": runtime,
	}
}

// equals reports whether a freshly computed identity matches the one a manifest
// recorded.
//
// The comparison runs over the canonical encoding rather than over the decoded
// values because the manifest side arrives as decoded JSON — json.Number for
// every number, nil for every null — and comparing those to Go's int64 and
// *string directly would need a type switch per field that drifts the moment
// the identity gains one.
func (i schemaIdentity) equals(recorded any) (bool, error) {
	mine, err := encodeCompact(i.payload())
	if err != nil {
		return false, err
	}
	theirs, err := encodeCompact(recorded)
	if err != nil {
		return false, err
	}
	return bytes.Equal(mine, theirs), nil
}

// readOnlyParameters open a database read-only twice over: mode=ro asks SQLite
// for a read-only file handle, and query_only refuses a write even if some
// future caller obtains a writable one. Inspection must be provably incapable
// of touching the file it is judging.
var readOnlyParameters = []string{"mode=ro", "_pragma=query_only(1)", "_pragma=busy_timeout(5000)"}

// copyParameters open the source of an online copy.
//
// query_only is deliberately absent: this driver refuses VACUUM INTO on a
// connection that carries it, even though the statement only writes the output
// file. mode=ro is the stronger guarantee anyway — the source is opened
// O_RDONLY, so no code path can mutate it. The longer busy timeout matches the
// Python track's 30-second copy timeout.
var copyParameters = []string{"mode=ro", "_pragma=busy_timeout(30000)"}

// openSQLite opens one SQLite file through a URI DSN.
//
// The path travels as a file:// URI so a directory containing '?' or '#'
// cannot be mistaken for query parameters, and the pool is capped at a single
// connection because every PRAGMA here is per-connection state.
func openSQLite(ctx context.Context, path string, parameters []string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve the database path %s: %w", path, err)
	}
	uri := url.URL{Scheme: "file", Path: absolute}
	pool, err := sql.Open(sqliteDriver, uri.String()+"?"+strings.Join(parameters, "&"))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	pool.SetMaxOpenConns(1)
	// sql.Open is lazy, so nothing has touched the file yet. Connect eagerly so
	// "this path is not a database" surfaces here, where the caller still knows
	// what it was trying to do.
	if err := pool.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("connect to %s: %w", filepath.Base(path), err), pool.Close())
	}
	return pool, nil
}

// inspectDatabase proves a SQLite file is intact, parseable by this binary, and
// reports the identity a manifest records for it.
func inspectDatabase(ctx context.Context, path string) (schemaIdentity, error) {
	identity, err := inspectOpenDatabase(ctx, path)
	if err != nil {
		if errors.Is(err, ErrSnapshot) {
			// A guard's own rejection carries the operator guidance and passes
			// through untouched; only driver and OS failures are generalized.
			return schemaIdentity{}, err
		}
		return schemaIdentity{}, fmt.Errorf("%w: Could not inspect SQLite database %s: %w", ErrSnapshot, path, err)
	}
	// Checked after the connection is closed, exactly as the Python track does:
	// the file is fine, this binary is simply too old to understand it.
	if identity.RuntimeVersion != nil && *identity.RuntimeVersion != domain.CurrentRuntimeSchemaVersion {
		return schemaIdentity{}, snapshotErrorf(
			"%s contains runtime schema version %q, but this application supports %q. "+
				"Upgrade the application or select a compatible snapshot.",
			filepath.Base(path), *identity.RuntimeVersion, domain.CurrentRuntimeSchemaVersion,
		)
	}
	return identity, nil
}

// inspectOpenDatabase runs the three checks that need an open connection.
func inspectOpenDatabase(ctx context.Context, path string) (identity schemaIdentity, err error) {
	pool, err := openSQLite(ctx, path, readOnlyParameters)
	if err != nil {
		return schemaIdentity{}, err
	}
	defer func() {
		if closeErr := pool.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close %s: %w", filepath.Base(path), closeErr))
		}
	}()

	// The full integrity check, not quick_check: a snapshot is the artifact an
	// operator restores from at the worst possible moment, and a cheaper check
	// that misses a corrupt index is worse than no check at all.
	var status string
	if scanErr := pool.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&status); scanErr != nil {
		return schemaIdentity{}, fmt.Errorf("run the integrity check on %s: %w", filepath.Base(path), scanErr)
	}
	if status != "ok" {
		return schemaIdentity{}, snapshotErrorf("SQLite integrity check failed for %s: %s", filepath.Base(path), status)
	}
	if auditErr := rejectUnsupportedAuditSchema(ctx, pool, filepath.Base(path)); auditErr != nil {
		return schemaIdentity{}, auditErr
	}
	return readSchemaIdentity(ctx, pool)
}

// rejectUnsupportedAuditSchema refuses audit rows this binary cannot parse.
//
// A newer binary may have written rows whose meaning this one does not know,
// and a snapshot of those rows is not something to publish over live state on
// a guess. The check runs on the copy during backup and on the snapshot during
// restore, so neither direction can smuggle one through.
func rejectUnsupportedAuditSchema(ctx context.Context, pool *sql.DB, filename string) error {
	var present int
	err := pool.QueryRowContext(ctx,
		`SELECT 1 FROM pragma_table_info('audit_log') WHERE name = 'schema_version'`,
	).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		// No version column at all is a legacy database, not a future one.
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect the audit_log schema_version column: %w", err)
	}

	var (
		value       any
		sqliteType  string
		unsupported = `
		SELECT schema_version, typeof(schema_version)
		FROM audit_log
		WHERE typeof(schema_version) != 'integer'
		   OR schema_version < 1
		   OR schema_version > ?
		ORDER BY id
		LIMIT 1`
	)
	err = pool.QueryRowContext(ctx, unsupported, domain.CurrentAuditSchemaVersion).Scan(&value, &sqliteType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("scan unsupported audit rows: %w", err)
	}
	return snapshotErrorf(
		"%s contains unsupported audit schema version %s with SQLite type %q; "+
			"this application supports integer versions from 1 through %d. "+
			"Upgrade the application or select a compatible snapshot.",
		filename, renderSQLiteValue(value), sqliteType, domain.CurrentAuditSchemaVersion,
	)
}

// renderSQLiteValue mirrors Python's %r on the offending value: quoted when it
// is text, bare when it is a number, so the message shows which of the two
// SQLite actually stored.
func renderSQLiteValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strconv.Quote(typed)
	case []byte:
		return strconv.Quote(string(typed))
	default:
		return fmt.Sprint(typed)
	}
}

// readSchemaIdentity fingerprints a database's schema and version markers.
func readSchemaIdentity(ctx context.Context, pool *sql.DB) (schemaIdentity, error) {
	digest, err := schemaDigest(ctx, pool)
	if err != nil {
		return schemaIdentity{}, err
	}
	identity := schemaIdentity{SchemaSHA256: digest}
	if scanErr := pool.QueryRowContext(ctx, "PRAGMA user_version").Scan(&identity.UserVersion); scanErr != nil {
		return schemaIdentity{}, fmt.Errorf("read the user_version pragma: %w", scanErr)
	}
	if identity.AuditVersion, err = maxAuditVersion(ctx, pool); err != nil {
		return schemaIdentity{}, err
	}
	if identity.RuntimeVersion, err = runtimeSchemaVersion(ctx, pool); err != nil {
		return schemaIdentity{}, err
	}
	return identity, nil
}

// schemaDigest hashes every user-defined schema object.
//
// The rows are serialized as a JSON array of arrays, which is what Python's
// fetchall() of tuples encodes to, so both tracks produce the same digest for
// the same schema.
func schemaDigest(ctx context.Context, pool *sql.DB) (string, error) {
	rows, err := pool.QueryContext(ctx, `
		SELECT type, name, tbl_name, COALESCE(sql, '')
		FROM sqlite_schema
		WHERE name NOT LIKE 'sqlite_%'
		ORDER BY type, name`)
	if err != nil {
		return "", fmt.Errorf("read the schema catalog: %w", err)
	}
	defer func() { _ = rows.Close() }()
	// A non-nil empty slice: an empty schema must encode as [] rather than null.
	catalog := [][]string{}
	for rows.Next() {
		var objectType, name, table, definition string
		if scanErr := rows.Scan(&objectType, &name, &table, &definition); scanErr != nil {
			return "", fmt.Errorf("scan the schema catalog: %w", scanErr)
		}
		catalog = append(catalog, []string{objectType, name, table, definition})
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return "", fmt.Errorf("read the schema catalog: %w", rowsErr)
	}
	encoded, err := encodeCompact(catalog)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// maxAuditVersion reports the highest audit schema version stored, or nil when
// the table predates the version column.
func maxAuditVersion(ctx context.Context, pool *sql.DB) (any, error) {
	present, err := hasColumns(ctx, pool, "audit_log", "schema_version")
	if err != nil || !present {
		return nil, err
	}
	var value any
	if err := pool.QueryRowContext(ctx, "SELECT MAX(schema_version) FROM audit_log").Scan(&value); err != nil {
		return nil, fmt.Errorf("read the audit schema version: %w", err)
	}
	if raw, ok := value.([]byte); ok {
		// database/sql hands text back as bytes; the manifest records a string.
		return string(raw), nil
	}
	return value, nil
}

// runtimeSchemaVersion reports ADK's own session schema marker, or nil when the
// runtime metadata table is absent or unversioned.
func runtimeSchemaVersion(ctx context.Context, pool *sql.DB) (*string, error) {
	present, err := hasColumns(ctx, pool, "adk_internal_metadata", "key", "value")
	if err != nil || !present {
		return nil, err
	}
	var version string
	err = pool.QueryRowContext(ctx,
		`SELECT value FROM adk_internal_metadata WHERE "key" = 'schema_version'`,
	).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the runtime schema version: %w", err)
	}
	return &version, nil
}

// hasColumns reports whether a table declares every named column.
func hasColumns(ctx context.Context, pool *sql.DB, table string, columns ...string) (bool, error) {
	var found int
	err := pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name IN (SELECT value FROM json_each(?))`,
		table, mustJSONList(columns),
	).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("inspect the %s columns: %w", table, err)
	}
	return found == len(columns), nil
}

// mustJSONList renders a fixed list of column names for json_each.
//
// The input is always a compile-time literal from this file, so the encoding
// cannot fail; a panic here would mean the binary is corrupt, not that the
// database is.
func mustJSONList(values []string) string {
	encoded, err := json.Marshal(values)
	if err != nil {
		panic(fmt.Sprintf("encode the column list %v: %v", values, err))
	}
	return string(encoded)
}

// copyDatabase publishes an online-safe copy of a live SQLite database.
//
// The Python track uses SQLite's backup API, which this driver does not expose;
// VACUUM INTO is the documented equivalent. It runs inside a read transaction,
// so a concurrent writer neither blocks it nor corrupts the copy, and it
// refuses to write over an existing file — the staging directory is always
// fresh, so that refusal is a safety net rather than a constraint.
func copyDatabase(ctx context.Context, source, target string) (err error) {
	pool, openErr := openSQLite(ctx, source, copyParameters)
	if openErr != nil {
		return fmt.Errorf("%w: Could not back up SQLite database %s: %w", ErrSnapshot, source, openErr)
	}
	defer func() {
		if closeErr := pool.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("%w: Could not back up SQLite database %s: %w",
				ErrSnapshot, source, closeErr))
		}
	}()
	if _, execErr := pool.ExecContext(ctx, "VACUUM INTO ?", target); execErr != nil {
		return fmt.Errorf("%w: Could not back up SQLite database %s: %w", ErrSnapshot, source, execErr)
	}
	return nil
}
