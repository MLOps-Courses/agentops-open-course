package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// runtimeTables are the tables a usable runtime database must carry, sorted so
// the "missing required tables" message is deterministic.
var runtimeTables = []string{"audit_log", "incidents", "services"}

// auditIdempotencyIndex is the unique index that makes a guarded write
// replay-safe. Its key is exactly (invocation_id, action, target); session_id
// and actor are deliberately outside it, because the same logical write
// redelivered under a new session must still be recognized as a replay.
const auditIdempotencyIndex = "uq_audit_log_idempotency"

// futureAuditSchemaGuidance is the operator-facing half of the future-schema
// refusal. It is a constant because all three doors — probe, read, migrate —
// must say the same thing, and the tests match on it.
const futureAuditSchemaGuidance = "The runtime database contains audit rows with an unsupported application schema. " +
	"Upgrade the application or select a compatible snapshot before reading or changing this state"

// indexColumn is one keyed column of pragma_index_xinfo.
type indexColumn struct {
	Name string
	Coll string
	// Seqno is the column's position in the key and Desc its sort direction;
	// both are ints because SQLite reports them as integers.
	Seqno int
	Desc  int
}

// auditIdempotencyKey is the exact index shape every guard verifies: three
// ascending columns collated BINARY, in this order.
//
// The collation is load-bearing rather than pedantic. A NOCASE index would let
// two invocation ids that differ only in case collide, silently swallowing a
// second genuine write as a replay; a partial index would let duplicates in
// through the excluded rows. Neither is repaired — a runtime database with the
// wrong index shape is a state an operator has to look at.
var auditIdempotencyKey = []indexColumn{
	{Seqno: 0, Name: "invocation_id", Desc: 0, Coll: "BINARY"},
	{Seqno: 1, Name: "action", Desc: 0, Coll: "BINARY"},
	{Seqno: 2, Name: "target", Desc: 0, Coll: "BINARY"},
}

// auditTrigger is one append-only guard and the exact body it must have.
type auditTrigger struct {
	Name string
	SQL  string
}

// auditTriggers make audit_log append-only at the database, not merely by
// convention in the code above it.
//
// An ordered slice, not a map: Go randomizes map iteration, and creating the
// two triggers in a stable order keeps a partial failure reproducible.
var auditTriggers = []auditTrigger{
	{
		Name: "audit_log_no_update",
		SQL: `CREATE TRIGGER audit_log_no_update
BEFORE UPDATE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only');
END`,
	},
	{
		Name: "audit_log_no_delete",
		SQL: `CREATE TRIGGER audit_log_no_delete
BEFORE DELETE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only');
END`,
	},
}

// columnDefinition is one row of pragma_table_info narrowed to the three fields
// the audit schema contract pins.
type columnDefinition struct {
	Type string
	// Default is nullable because a column without a DEFAULT reports NULL, and
	// SQLite renders the default it does have as text — the audit column's is
	// the string "1", never the integer 1.
	Default sql.NullString
	NotNull int
}

// rejectFutureAuditSchema refuses audit rows this binary cannot parse, without
// changing anything.
//
// It is the first thing every door does — probe, read, and migrate — because a
// newer binary may have written rows whose meaning this one does not know. The
// safe response is to stop, not to guess.
func rejectFutureAuditSchema(ctx context.Context, q querier) error {
	var present int
	err := q.QueryRowContext(ctx,
		`SELECT 1 FROM pragma_table_info('audit_log') WHERE name = 'schema_version'`,
	).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		// A legacy database has no version column at all, and that is not a
		// future schema. It is handled where it belongs: the migration adds the
		// column, and the probe reports it through the "not prepared" path.
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
	err = q.QueryRowContext(ctx, unsupported, domain.CurrentAuditSchemaVersion).Scan(&value, &sqliteType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("scan unsupported audit rows: %w", err)
	}
	return fmt.Errorf("%w: %s: found %s with SQLite type %q; supported versions are integers from 1 through %d",
		ErrDataAccess, futureAuditSchemaGuidance, renderSQLiteValue(value), sqliteType, domain.CurrentAuditSchemaVersion)
}

// renderSQLiteValue mirrors Python's %r on the offending value: quoted when it
// is text, bare when it is a number, so the message shows which of the two
// SQLite actually stored.
func renderSQLiteValue(value any) string {
	if text, ok := value.(string); ok {
		return strconv.Quote(text)
	}
	return fmt.Sprint(value)
}

// auditIdempotencyIndexIsCurrent reports whether the named index enforces the
// exact bytewise key contract.
func auditIdempotencyIndexIsCurrent(ctx context.Context, q querier) (bool, error) {
	var unique, partial int
	err := q.QueryRowContext(ctx,
		`SELECT "unique", partial FROM pragma_index_list('audit_log') WHERE name = ?`,
		auditIdempotencyIndex,
	).Scan(&unique, &partial)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect the %s index: %w", auditIdempotencyIndex, err)
	}
	if unique != 1 || partial != 0 {
		return false, nil
	}

	rows, err := q.QueryContext(ctx,
		`SELECT seqno, name, "desc", coll FROM pragma_index_xinfo(?) WHERE key = 1 ORDER BY seqno`,
		auditIdempotencyIndex,
	)
	if err != nil {
		return false, fmt.Errorf("inspect the %s key columns: %w", auditIdempotencyIndex, err)
	}
	defer func() { _ = rows.Close() }()
	var key []indexColumn
	for rows.Next() {
		var column indexColumn
		if err := rows.Scan(&column.Seqno, &column.Name, &column.Desc, &column.Coll); err != nil {
			return false, fmt.Errorf("scan the %s key columns: %w", auditIdempotencyIndex, err)
		}
		key = append(key, column)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read the %s key columns: %w", auditIdempotencyIndex, err)
	}
	return slices.Equal(key, auditIdempotencyKey), nil
}

// normalizedSchemaSQL normalizes the whitespace SQLite owns without weakening
// the schema contract.
//
// SQLite stores a trigger's source text verbatim, so a trigger created by this
// package and one created by sql/schema.sql differ in indentation while meaning
// exactly the same thing. Collapsing runs of whitespace and lower-casing
// forgives that and nothing else — a changed table, a changed RAISE, or an
// extra statement all still fail the comparison.
func normalizedSchemaSQL(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

// auditTriggersAreCurrent reports whether both append-only triggers carry their
// exact reviewed bodies.
func auditTriggersAreCurrent(ctx context.Context, q querier) (bool, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name, sql FROM sqlite_schema WHERE type = 'trigger' AND name LIKE 'audit_log_no_%'`,
	)
	if err != nil {
		return false, fmt.Errorf("inspect the append-only triggers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	found := make(map[string]sql.NullString, len(auditTriggers))
	for rows.Next() {
		var (
			name string
			body sql.NullString
		)
		if err := rows.Scan(&name, &body); err != nil {
			return false, fmt.Errorf("scan the append-only triggers: %w", err)
		}
		found[name] = body
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read the append-only triggers: %w", err)
	}
	if len(found) != len(auditTriggers) {
		// An extra audit_log_no_* trigger counts as a mismatch too: something
		// nobody reviewed is guarding the audit trail.
		return false, nil
	}
	for _, trigger := range auditTriggers {
		body, ok := found[trigger.Name]
		if !ok || !body.Valid || normalizedSchemaSQL(body.String) != normalizedSchemaSQL(trigger.SQL) {
			return false, nil
		}
	}
	return true, nil
}

// prepareAuditTriggers creates missing append-only triggers and refuses to
// touch a conflicting one.
//
// Creating is safe — a missing trigger is a strictly weaker database than the
// contract, and adding it back only tightens it. Replacing is not: a trigger
// with a different body was put there by somebody, and dropping it would
// destroy whatever protection they intended.
func prepareAuditTriggers(ctx context.Context, tx *sql.Tx) error {
	for _, trigger := range auditTriggers {
		var body sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT sql FROM sqlite_schema WHERE type = 'trigger' AND name = ?`, trigger.Name,
		).Scan(&body)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, execErr := tx.ExecContext(ctx, trigger.SQL); execErr != nil {
				return fmt.Errorf("create the %s trigger: %w", trigger.Name, execErr)
			}
		case err != nil:
			return fmt.Errorf("inspect the %s trigger: %w", trigger.Name, err)
		case !body.Valid || normalizedSchemaSQL(body.String) != normalizedSchemaSQL(trigger.SQL):
			return fmt.Errorf("%w: Runtime trigger %q has an unexpected definition; "+
				"inspect the runtime database before retrying the migration", ErrDataAccess, trigger.Name)
		}
	}
	return nil
}

// probeReport is everything one read-only pass collected.
//
// The verdicts are computed from it afterwards, with the connection already
// closed, because Python decided outside its `with` block too: a probe that
// fails must not leave a handle open on the state it just refused.
type probeReport struct {
	tables             map[string]struct{}
	versionColumn      *columnDefinition
	indexIsCurrent     bool
	triggersAreCurrent bool
}

// missingTables lists the required tables the database does not have, sorted.
func (r *probeReport) missingTables() []string {
	var missing []string
	for _, table := range runtimeTables {
		if _, ok := r.tables[table]; !ok {
			missing = append(missing, table)
		}
	}
	return missing
}

// auditSchemaIsPrepared reports whether the audit contract holds in full: the
// version column with its exact declaration, the exact index, and both
// triggers. Any one of them missing makes the database unsafe to append to.
func (r *probeReport) auditSchemaIsPrepared() bool {
	if r.versionColumn == nil {
		return false
	}
	return strings.EqualFold(r.versionColumn.Type, "INTEGER") &&
		r.versionColumn.NotNull == 1 &&
		r.versionColumn.Default.Valid &&
		r.versionColumn.Default.String == strconv.Itoa(domain.CurrentAuditSchemaVersion) &&
		r.indexIsCurrent &&
		r.triggersAreCurrent
}

// ProbeRuntimeDatabase verifies initialized runtime state read-only.
//
// It never migrates and never repairs. It is the health check a server runs at
// startup and the MCP server runs before serving, so its only job is to say
// whether the state on disk is safe to use — and to leave every byte of it
// alone while deciding. It looks at the state directory only: the committed
// seed is not runtime state and is never probed.
func (s *Store) ProbeRuntimeDatabase(ctx context.Context) (string, error) {
	if err := s.verifyStateDirectory(); err != nil {
		return "", err
	}
	path := s.RuntimePath()
	if err := verifyRegularPath(path); err != nil {
		return "", fmt.Errorf("%w: Runtime database is not initialized as a regular file: %s: %w",
			ErrDataAccess, path, err)
	}
	report, err := s.probe(ctx, path)
	if err != nil {
		// A guard's own refusal, the future-schema guidance above all, reaches
		// the operator verbatim and outranks every check below: those checks
		// would describe a symptom, and it describes the cause.
		if errors.Is(err, ErrDataAccess) {
			return "", err
		}
		return "", fmt.Errorf("%w: Runtime database read-only probe failed for %s: %w",
			ErrDataAccess, filepath.Base(path), err)
	}
	if missing := report.missingTables(); len(missing) > 0 {
		return "", fmt.Errorf("%w: Runtime database is missing required tables: %s",
			ErrDataAccess, strings.Join(missing, ", "))
	}
	if !report.auditSchemaIsPrepared() {
		return "", fmt.Errorf("%w: Runtime database audit schema is not prepared: expected the current "+
			"schema_version and exact ascending BINARY unique index %q plus append-only triggers",
			ErrDataAccess, auditIdempotencyIndex)
	}
	return path, nil
}

// probe collects the report over one read-only connection.
func (s *Store) probe(ctx context.Context, path string) (report *probeReport, err error) {
	db, err := openDatabaseWithHook(ctx, path, true, s.beforeDatabaseConnect)
	if err != nil {
		return nil, err
	}
	defer func() { err = db.closeWith(err) }()

	var integrity string
	switch scanErr := db.QueryRowContext(ctx, "PRAGMA quick_check(1)").Scan(&integrity); {
	case errors.Is(scanErr, sql.ErrNoRows):
		integrity = "no result"
	case scanErr != nil:
		return nil, fmt.Errorf("run the integrity check: %w", scanErr)
	}
	if integrity != "ok" {
		return nil, fmt.Errorf("%w: Runtime database integrity check failed for %s: %s",
			ErrDataAccess, db.name, integrity)
	}

	tables, err := presentTables(ctx, db)
	if err != nil {
		return nil, err
	}
	versionColumn, err := auditVersionColumn(ctx, db)
	if err != nil {
		return nil, err
	}
	indexIsCurrent, err := auditIdempotencyIndexIsCurrent(ctx, db)
	if err != nil {
		return nil, err
	}
	triggersAreCurrent, err := auditTriggersAreCurrent(ctx, db)
	if err != nil {
		return nil, err
	}
	// Last, so its verdict is the one that surfaces when a database is both
	// unprepared and unreadable.
	if err := rejectFutureAuditSchema(ctx, db); err != nil {
		return nil, err
	}
	return &probeReport{
		tables:             tables,
		versionColumn:      versionColumn,
		indexIsCurrent:     indexIsCurrent,
		triggersAreCurrent: triggersAreCurrent,
	}, nil
}

// presentTables returns which of the required tables the database has.
//
// Python narrowed the set with an IN clause listing the three names. Selecting
// every table and intersecting in Go keeps runtimeTables the single place that
// list is written, which matters because the same slice also builds the
// "missing required tables" message.
func presentTables(ctx context.Context, q querier) (map[string]struct{}, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type = 'table'`)
	if err != nil {
		return nil, fmt.Errorf("list the runtime tables: %w", err)
	}
	defer func() { _ = rows.Close() }()
	tables := make(map[string]struct{}, len(runtimeTables))
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan the runtime tables: %w", err)
		}
		if slices.Contains(runtimeTables, name) {
			tables[name] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the runtime tables: %w", err)
	}
	return tables, nil
}

// auditVersionColumn returns the declaration of audit_log.schema_version, or
// nil when the column does not exist.
func auditVersionColumn(ctx context.Context, q querier) (*columnDefinition, error) {
	var column columnDefinition
	err := q.QueryRowContext(ctx,
		`SELECT type, "notnull", dflt_value FROM pragma_table_info('audit_log') WHERE name = 'schema_version'`,
	).Scan(&column.Type, &column.NotNull, &column.Default)
	if errors.Is(err, sql.ErrNoRows) {
		// A nil column is the answer, not a failure: a legacy audit_log simply
		// does not have the column yet, and the caller decides what to do.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect the audit_log schema_version column: %w", err)
	}
	return &column, nil
}

// PrepareRuntimeDatabase publishes runtime state if needed and brings its audit
// schema up to the version this binary writes.
//
// The migration is strictly additive — it adds a column, recreates missing
// triggers, and creates the unique index — and it is idempotent, so a second
// run commits an empty transaction. Everything else it can only refuse: a
// column with the wrong declaration, a trigger with a different body, an index
// with the wrong shape, or existing rows that would violate the index are all
// states a human has to resolve.
func (s *Store) PrepareRuntimeDatabase(ctx context.Context) (string, error) {
	path, err := s.DBPath()
	if err != nil {
		return "", err
	}
	db, err := openDatabaseWithHook(ctx, path, false, s.beforeDatabaseConnect)
	if err != nil {
		return "", fmt.Errorf("%w: Could not open database: %s: %w", ErrDataAccess, filepath.Base(path), err)
	}
	if err := db.closeWith(db.fail(migrate(ctx, db))); err != nil {
		return "", err
	}
	return path, nil
}

// migrate runs the whole check-and-upgrade inside one BEGIN IMMEDIATE.
//
// One transaction for the guards and the DDL together is the point: SQLite runs
// ALTER TABLE ADD COLUMN and CREATE UNIQUE INDEX transactionally, so a refusal
// halfway through leaves a database that looks exactly as it did before. The
// duplicate-key scan in particular has to see the same snapshot the index
// creation will.
func migrate(ctx context.Context, db *database) error {
	return db.inWriteTransaction(ctx, func(tx *sql.Tx) (bool, error) {
		if err := rejectFutureAuditSchema(ctx, tx); err != nil {
			return false, err
		}
		if err := prepareAuditVersionColumn(ctx, tx); err != nil {
			return false, err
		}
		if err := prepareAuditTriggers(ctx, tx); err != nil {
			return false, err
		}
		if err := prepareAuditIdempotencyIndex(ctx, tx); err != nil {
			return false, err
		}
		// Committing even when nothing changed keeps the migration idempotent:
		// a second run over a current schema is an empty transaction, not a
		// special case somebody has to remember.
		return true, nil
	})
}

// prepareAuditVersionColumn adds audit_log.schema_version when it is absent and
// verifies its declaration either way.
func prepareAuditVersionColumn(ctx context.Context, tx *sql.Tx) error {
	column, err := auditVersionColumn(ctx, tx)
	if err != nil {
		return err
	}
	if column == nil {
		// NOT NULL DEFAULT 1 backfills every existing row with the version this
		// binary understands, which is the correct reading of a row written
		// before the column existed.
		if _, execErr := tx.ExecContext(ctx, `
			ALTER TABLE audit_log
			ADD COLUMN schema_version INTEGER NOT NULL DEFAULT 1 CHECK (schema_version >= 1)`,
		); execErr != nil {
			return fmt.Errorf("add the audit_log schema_version column: %w", execErr)
		}
		if column, err = auditVersionColumn(ctx, tx); err != nil {
			return err
		}
	}
	if column == nil ||
		!strings.EqualFold(column.Type, "INTEGER") ||
		column.NotNull != 1 ||
		!column.Default.Valid ||
		column.Default.String != strconv.Itoa(domain.CurrentAuditSchemaVersion) {
		return fmt.Errorf("%w: Runtime audit schema_version has an unexpected definition; "+
			"inspect the runtime database before retrying the migration", ErrDataAccess)
	}
	return nil
}

// prepareAuditIdempotencyIndex creates the unique index, or refuses.
func prepareAuditIdempotencyIndex(ctx context.Context, tx *sql.Tx) error {
	var present int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM pragma_index_list('audit_log') WHERE name = ?`, auditIdempotencyIndex,
	).Scan(&present)
	switch {
	case err == nil:
		current, currentErr := auditIdempotencyIndexIsCurrent(ctx, tx)
		if currentErr != nil {
			return currentErr
		}
		if !current {
			return fmt.Errorf("%w: Runtime index %q has an unexpected definition; "+
				"inspect the runtime database before retrying the migration", ErrDataAccess, auditIdempotencyIndex)
		}
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("inspect the %s index: %w", auditIdempotencyIndex, err)
	}

	// Scan for duplicates before creating the index rather than letting CREATE
	// UNIQUE INDEX fail: SQLite's own error names one row, and an operator
	// reconciling an audit trail needs the key and how many rows carry it.
	var (
		invocationID, action, target string
		rowCount                     int64
	)
	err = tx.QueryRowContext(ctx, `
		SELECT invocation_id, action, target, COUNT(*) AS row_count
		FROM audit_log
		GROUP BY invocation_id, action, target
		HAVING COUNT(*) > 1
		ORDER BY row_count DESC, invocation_id, action, target
		LIMIT 1`,
	).Scan(&invocationID, &action, &target, &rowCount)
	switch {
	case err == nil:
		return fmt.Errorf("%w: Runtime database contains a duplicate audit idempotency key: "+
			"invocation_id=%q, action=%q, target=%q has %d rows. "+
			"Export and reconcile duplicate audit rows before retrying the migration",
			ErrDataAccess, invocationID, action, target, rowCount)
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("scan for duplicate audit idempotency keys: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		CREATE UNIQUE INDEX uq_audit_log_idempotency
		ON audit_log (invocation_id, action, target)`,
	); err != nil {
		return fmt.Errorf("create the %s index: %w", auditIdempotencyIndex, err)
	}
	return nil
}
