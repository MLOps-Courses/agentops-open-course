package a2aserver

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// TaskDatabaseName is the A2A task store file inside AGENT_STATE_DIR.
//
// It is a second database beside ADK's runtime.db rather than a table inside
// it, and that is a deliberate divergence from the Python track. Python could
// share one connection pool because both stores were SQLAlchemy engines it
// owned; ADK Go's session service keeps its *gorm.DB private, so a task store
// inside runtime.db would mean a second, independent pool on one SQLite file —
// exactly the "database is locked" race the Python comment warns against.
// Separate files give each store its own single-writer lock and no contention
// at all, and the state snapshot tooling publishes every *.db in the directory,
// so a restore still moves the two together as one generation.
const TaskDatabaseName = "tasks.db"

// The task store's own schema. `store_metadata` is the marker Python read from
// ADK Python's `adk_internal_metadata` table: ADK Go's gorm schema has no
// version marker of any kind, so the guarantee — refuse a state generation this
// binary does not understand, before startup writes anything — is re-earned on
// the one database this package owns.
const (
	tasksTable         = "tasks"
	storeMetadataTable = "store_metadata"
	schemaVersionKey   = "schema_version"
)

// taskStoreTables is the complete set of tables a prepared task store has. It
// is the list the preflight and readiness checks compare against, so it is
// written once.
var taskStoreTables = []string{tasksTable, storeMetadataTable}

// taskStoreColumns is the required shape of the tasks table.
//
// Only `id`, `context_id` and the JSON document are protocol data; `owner`,
// `tenant`, `state`, `version`, and the two timestamps exist because
// [taskstore.Store.List] filters, orders and paginates on them and
// [taskstore.StoredTask] does not carry them.
var taskStoreColumns = []string{
	"id", "context_id", "owner", "tenant", "state",
	"version", "last_updated_ns", "status_timestamp_ns", "task",
}

// taskStoreSchema is the DDL a fresh store is created with. Every statement is
// IF NOT EXISTS so opening an existing store is the same code path.
var taskStoreSchema = []string{
	`CREATE TABLE IF NOT EXISTS store_metadata (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS tasks (
		id                  TEXT    PRIMARY KEY,
		context_id          TEXT    NOT NULL,
		owner               TEXT    NOT NULL,
		tenant              TEXT    NOT NULL,
		state               TEXT    NOT NULL,
		version             INTEGER NOT NULL,
		last_updated_ns     INTEGER NOT NULL,
		status_timestamp_ns INTEGER,
		task                TEXT    NOT NULL
	)`,
	// The listing index covers the exact predicate and ordering List uses:
	// owner first because every listing is scoped to one caller, then the sort
	// key. Without it a listing is a full scan plus a sort.
	`CREATE INDEX IF NOT EXISTS tasks_owner_recent
		ON tasks (owner, last_updated_ns DESC, id DESC)`,
	`CREATE INDEX IF NOT EXISTS tasks_context ON tasks (context_id)`,
}

// List paging bounds, copied from the in-memory reference implementation so a
// client cannot tell the two stores apart.
const (
	defaultPageSize = 50
	maxPageSize     = 100
	// defaultMaxHistoryLength is how many trailing history messages a listed
	// task carries when the request does not say.
	defaultMaxHistoryLength = 100
)

// ErrTaskStore marks a failure of the store itself — the file, the driver, the
// schema — as opposed to the protocol outcomes the interface documents.
//
// Protocol outcomes (a2a.ErrTaskNotFound, taskstore.ErrTaskAlreadyExists,
// taskstore.ErrConcurrentModification, a2a.ErrUnauthenticated,
// a2a.ErrInvalidRequest, a2a.ErrParseError) are wrapped bare so
// a2a.ErrorReason's errors.Is walk still maps them to the right wire reason.
var ErrTaskStore = errors.New("task store failed")

// TaskStoreConfig is everything [OpenTaskStore] needs.
type TaskStoreConfig struct {
	// Authenticator names the caller a task belongs to. Nil uses
	// a2asrv.NewTaskStoreAuthenticator, which reads the A2A call context —
	// the same default the in-memory store is installed with.
	Authenticator taskstore.Authenticator

	// Tenant resolves the agent-owner scope a task is created under. Nil reads
	// it from the A2A call context. See [TaskStore.List] for why this exists at
	// all: a2a.ListTasksRequest carries a Tenant filter, and a store that
	// ignored it would answer a scoped question with unscoped data.
	Tenant func(ctx context.Context) string

	// Now stamps last_updated. Nil uses time.Now. It is a seam because the
	// listing order and the page-token cursor are both derived from it, and a
	// test that could not control it would be timing-dependent.
	Now func() time.Time

	// Path is the SQLite file. Required. Its parent directory must exist.
	Path string
}

// TaskStore is the persistent [taskstore.Store] the deployment contract needs.
//
// ADK Go ships only taskstore.NewInMemory, whose contents do not survive a
// restart; a kagent BYO deployment that lost every in-flight task on a rolling
// update would not be a deployment contract at all. This implementation is
// behavior-compatible with the in-memory one — the same sentinels, the same
// filters, the same ordering, the same page-token codec — and adds durability.
//
// The zero value is not usable; construct one with [OpenTaskStore].
type TaskStore struct {
	pool          *sql.DB
	authenticator taskstore.Authenticator
	tenant        func(ctx context.Context) string
	now           func() time.Time
	path          string
}

// TaskStore implements the contract the A2A request handler installs.
var _ taskstore.Store = (*TaskStore)(nil)

// OpenTaskStore opens or creates the task database at cfg.Path.
//
// A store whose schema_version is not the one this binary writes is refused
// here rather than migrated: a snapshot from a newer binary is an operator
// decision, and silently reading it would be the one failure mode the whole
// snapshot/restore chapter exists to prevent.
func OpenTaskStore(ctx context.Context, cfg TaskStoreConfig) (*TaskStore, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("%w: Path is required", ErrTaskStore)
	}
	store := &TaskStore{
		path:          cfg.Path,
		authenticator: cfg.Authenticator,
		tenant:        cfg.Tenant,
		now:           cfg.Now,
	}
	if store.authenticator == nil {
		store.authenticator = a2asrv.NewTaskStoreAuthenticator()
	}
	if store.tenant == nil {
		store.tenant = callContextTenant
	}
	if store.now == nil {
		store.now = time.Now
	}

	pool, err := openSQLite(ctx, cfg.Path, false)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTaskStore, err)
	}
	if err := inWriteTransaction(ctx, pool, func(tx *sql.Tx) error {
		return prepareTaskSchema(ctx, tx)
	}); err != nil {
		return nil, closeWith(pool, filepath.Base(cfg.Path), fmt.Errorf("%w: %w", ErrTaskStore, err))
	}
	store.pool = pool
	return store, nil
}

// callContextTenant reads the tenant the A2A transport recorded for this call.
func callContextTenant(ctx context.Context) string {
	if callCtx, ok := a2asrv.CallContextFrom(ctx); ok {
		return callCtx.Tenant()
	}
	return ""
}

// prepareTaskSchema creates the schema when it is absent and refuses one this
// binary does not understand.
//
// Both halves run inside the caller's single BEGIN IMMEDIATE, so two processes
// racing to create the same fresh store cannot both decide it is empty.
func prepareTaskSchema(ctx context.Context, tx *sql.Tx) error {
	tables, err := presentTables(ctx, tx)
	if err != nil {
		return err
	}
	if len(tables) > 0 {
		if err := checkTaskSchema(ctx, tx, tables); err != nil {
			return err
		}
	}
	for _, statement := range taskStoreSchema {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create the task store schema: %w", err)
		}
	}
	// Idempotent: an existing marker was already checked above, so writing the
	// current version back is either the first write or a no-op.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO store_metadata (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		schemaVersionKey, domain.CurrentRuntimeSchemaVersion,
	); err != nil {
		return fmt.Errorf("stamp the task store schema version: %w", err)
	}
	return nil
}

// checkTaskSchema refuses a non-empty task database this binary cannot use.
//
// The messages are the Python track's, verbatim in their operative half, so an
// operator who has read Chapter 6 recognizes them: an unknown version says
// "upgrade the application or select a compatible snapshot", and a version that
// matches but is missing structure says "incomplete current schema".
func checkTaskSchema(ctx context.Context, tx *sql.Tx, tables map[string]struct{}) error {
	if _, ok := tables[storeMetadataTable]; !ok {
		return fmt.Errorf(
			"%s has an unsupported legacy or malformed schema. "+
				"Upgrade the application or select a compatible snapshot", TaskDatabaseName,
		)
	}
	var version string
	err := tx.QueryRowContext(ctx,
		`SELECT value FROM store_metadata WHERE key = ?`, schemaVersionKey).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		version = ""
	} else if err != nil {
		return fmt.Errorf("read the task store schema version: %w", err)
	}
	if version != domain.CurrentRuntimeSchemaVersion {
		return fmt.Errorf(
			"%s has runtime schema %q, but this application supports %q. "+
				"Upgrade the application or select a compatible snapshot",
			TaskDatabaseName, version, domain.CurrentRuntimeSchemaVersion,
		)
	}
	return checkTaskTables(ctx, tx, tables)
}

// checkTaskTables reports the tables and columns a current-version store is
// missing.
func checkTaskTables(ctx context.Context, queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, tables map[string]struct{},
) error {
	var missing []string
	for _, table := range taskStoreTables {
		if _, ok := tables[table]; !ok {
			missing = append(missing, table)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"%s has an incomplete current schema and is missing tables: %s. "+
				"Select a compatible snapshot before startup",
			TaskDatabaseName, strings.Join(missing, ", "),
		)
	}
	columns, err := tableColumns(ctx, queryer, tasksTable)
	if err != nil {
		return err
	}
	missing = nil
	for _, column := range taskStoreColumns {
		if _, ok := columns[column]; !ok {
			missing = append(missing, column)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"%s has an incomplete current schema: table %q is missing columns %s. "+
				"Select a compatible snapshot before startup",
			TaskDatabaseName, tasksTable, strings.Join(missing, ", "),
		)
	}
	return nil
}

// Path returns the database file this store is bound to.
func (s *TaskStore) Path() string { return s.path }

// Close releases the connection pool.
//
// [taskstore.Store] declares no Close, so the lifecycle is entirely the
// caller's; [Server] owns it for the served process.
func (s *TaskStore) Close() error {
	if err := s.pool.Close(); err != nil {
		return fmt.Errorf("%w: close %s: %w", ErrTaskStore, filepath.Base(s.path), err)
	}
	return nil
}

// Create implements [taskstore.Store].
//
// The order of the three preliminary steps matches the in-memory reference —
// validate, authenticate, encode — so a task with unusable metadata is refused
// before authentication is even attempted, and a caller that mutates its task
// after the call cannot reach stored state.
func (s *TaskStore) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	if err := validateTask(task); err != nil {
		return taskstore.TaskVersionMissing, err
	}
	if task == nil {
		// validateTask accepts a nil task (the reference does too), but there is
		// nothing to key a row on. Refusing here is the honest answer.
		return taskstore.TaskVersionMissing, fmt.Errorf("%w: Create requires a task", ErrTaskStore)
	}
	owner, err := s.authenticator(ctx)
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("taskstore auth failed: %w", err)
	}
	encoded, err := json.Marshal(task)
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("%w: encode task %q: %w", ErrTaskStore, task.ID, err)
	}

	const version = taskstore.TaskVersion(1)
	err = inWriteTransaction(ctx, s.pool, func(tx *sql.Tx) error {
		var exists int
		switch scanErr := tx.QueryRowContext(ctx,
			`SELECT 1 FROM tasks WHERE id = ?`, string(task.ID)).Scan(&exists); {
		case scanErr == nil:
			// Inside BEGIN IMMEDIATE, so this observation cannot be overtaken by
			// another writer before the insert below.
			return taskstore.ErrTaskAlreadyExists
		case !errors.Is(scanErr, sql.ErrNoRows):
			return fmt.Errorf("look up task %q: %w", task.ID, scanErr)
		}
		_, execErr := tx.ExecContext(ctx,
			`INSERT INTO tasks
				(id, context_id, owner, tenant, state, version, last_updated_ns, status_timestamp_ns, task)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(task.ID), task.ContextID, owner, s.tenant(ctx), string(task.Status.State),
			int64(version), s.now().UnixNano(), statusTimestamp(task), string(encoded),
		)
		if execErr != nil {
			return fmt.Errorf("insert task %q: %w", task.ID, execErr)
		}
		return nil
	})
	switch {
	case errors.Is(err, taskstore.ErrTaskAlreadyExists):
		return taskstore.TaskVersionMissing, err
	case err != nil:
		return taskstore.TaskVersionMissing, fmt.Errorf("%w: %w", ErrTaskStore, err)
	}
	return version, nil
}

// Update implements [taskstore.Store].
//
// The version read, the concurrency check and the row write are one
// BEGIN IMMEDIATE transaction. Splitting them would make the optimistic
// concurrency control decorative: two writers would both read version N, both
// find it current, and both write N+1.
//
// req.Event and req.PrevTask are deliberately unused, exactly as the in-memory
// reference leaves them: the desired state in req.Task already carries the
// history the event produced.
func (s *TaskStore) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	if req == nil || req.Task == nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("%w: Update requires a task", ErrTaskStore)
	}
	if err := validateTask(req.Task); err != nil {
		return taskstore.TaskVersionMissing, err
	}
	owner, err := s.authenticator(ctx)
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("taskstore auth failed: %w", err)
	}
	task := req.Task
	encoded, err := json.Marshal(task)
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("%w: encode task %q: %w", ErrTaskStore, task.ID, err)
	}

	var next taskstore.TaskVersion
	err = inWriteTransaction(ctx, s.pool, func(tx *sql.Tx) error {
		var stored int64
		switch scanErr := tx.QueryRowContext(ctx,
			`SELECT version FROM tasks WHERE id = ? AND owner = ?`, string(task.ID), owner).Scan(&stored); {
		case errors.Is(scanErr, sql.ErrNoRows):
			return a2a.ErrTaskNotFound
		case scanErr != nil:
			return fmt.Errorf("look up task %q: %w", task.ID, scanErr)
		}
		// TaskVersionMissing means "the caller is not tracking versions", which
		// the interface documents as skipping the check entirely rather than as
		// a mismatch against version zero.
		if req.PrevVersion != taskstore.TaskVersionMissing && taskstore.TaskVersion(stored) != req.PrevVersion {
			return taskstore.ErrConcurrentModification
		}
		next = taskstore.TaskVersion(stored) + 1
		// The owner column is never in the SET list: a task belongs to whoever
		// created it. The owner predicate also keeps this write scoped if the
		// transaction code is changed later and no longer holds one write lock.
		result, execErr := tx.ExecContext(ctx,
			`UPDATE tasks
			    SET context_id = ?, state = ?, version = ?, last_updated_ns = ?,
			        status_timestamp_ns = ?, task = ?
			  WHERE id = ? AND owner = ? AND version = ?`,
			task.ContextID, string(task.Status.State), int64(next), s.now().UnixNano(),
			statusTimestamp(task), string(encoded), string(task.ID), owner, stored,
		)
		if execErr != nil {
			return fmt.Errorf("update task %q: %w", task.ID, execErr)
		}
		affected, execErr := result.RowsAffected()
		if execErr != nil {
			return fmt.Errorf("count the rows updated for task %q: %w", task.ID, execErr)
		}
		if affected != 1 {
			// Unreachable while the transaction holds the write lock, and
			// reported anyway: "unreachable" is a claim about today's code, and
			// silently returning a version nobody wrote would be worse.
			return fmt.Errorf("updating task %q affected %d rows, want 1", task.ID, affected)
		}
		return nil
	})
	switch {
	case errors.Is(err, a2a.ErrTaskNotFound), errors.Is(err, taskstore.ErrConcurrentModification):
		return taskstore.TaskVersionMissing, err
	case err != nil:
		return taskstore.TaskVersionMissing, fmt.Errorf("%w: %w", ErrTaskStore, err)
	}
	return next, nil
}

// Get implements [taskstore.Store].
//
// The returned task is decoded from its stored bytes on every call, so a caller
// that mutates the result cannot reach stored state — the JSON round trip is
// what the in-memory store needs an explicit deep copy for.
func (s *TaskStore) Get(ctx context.Context, taskID a2a.TaskID) (*taskstore.StoredTask, error) {
	owner, err := s.authenticator(ctx)
	if err != nil {
		return nil, fmt.Errorf("taskstore auth failed: %w", err)
	}
	var (
		encoded string
		version int64
	)
	err = s.pool.QueryRowContext(ctx,
		`SELECT task, version FROM tasks WHERE id = ? AND owner = ?`, string(taskID), owner).
		Scan(&encoded, &version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, a2a.ErrTaskNotFound
	case err != nil:
		return nil, fmt.Errorf("%w: look up task %q: %w", ErrTaskStore, taskID, err)
	}
	task, err := decodeTask(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTaskStore, err)
	}
	return &taskstore.StoredTask{Task: task, Version: taskstore.TaskVersion(version), User: owner}, nil
}

// List implements [taskstore.Store].
//
// Every filter a2a.ListTasksRequest carries is honored, and all of them except
// the history and artifact trimming are honored in SQL so a large store does
// not have to be read into memory to answer a scoped question:
//
//   - Tenant and ContextID and Status are equality predicates.
//   - StatusTimestampAfter drops older rows but keeps rows with no status
//     timestamp at all, which is what the in-memory reference does.
//   - PageSize bounds the page, PageToken is a (last-updated, id) cursor.
//   - HistoryLength trims each result's trailing history; IncludeArtifacts
//     drops artifacts unless asked for.
//
// TotalSize counts the filtered rows before pagination, so a caller can tell
// "one page of many" from "one page in total".
func (s *TaskStore) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: List requires a request", ErrTaskStore)
	}
	owner, err := s.authenticator(ctx)
	if owner == "" || err != nil {
		// Matching the reference exactly: an unnamed caller cannot be shown any
		// task, because task ownership is the only access control this store has.
		return nil, a2a.ErrUnauthenticated
	}
	pageSize := req.PageSize
	switch {
	case pageSize == 0:
		pageSize = defaultPageSize
	case pageSize < 1 || pageSize > maxPageSize:
		return nil, fmt.Errorf("page size must be between 1 and %d inclusive, got %d: %w",
			maxPageSize, pageSize, a2a.ErrInvalidRequest)
	}

	filters, err := listFilters(owner, req)
	if err != nil {
		return nil, err
	}
	var response *a2a.ListTasksResponse
	err = inWriteTransaction(ctx, s.pool, func(tx *sql.Tx) error {
		// The count and the page run in one transaction so a concurrent write
		// cannot make TotalSize describe a different set than the page does.
		var total int
		if countErr := tx.QueryRowContext(ctx, countTasksQuery, filters.match()...).Scan(&total); countErr != nil {
			return fmt.Errorf("count the matching tasks: %w", countErr)
		}
		// One row beyond the page is what says whether a next page exists,
		// without a second count.
		rows, queryErr := tx.QueryContext(ctx, listTasksQuery, filters.page(pageSize+1)...)
		if queryErr != nil {
			return fmt.Errorf("list the matching tasks: %w", queryErr)
		}
		defer func() { _ = rows.Close() }()

		var (
			tasks         []*a2a.Task
			nextPageToken string
			seen          int
			lastUpdated   int64
			lastID        string
		)
		for rows.Next() {
			seen++
			if seen > pageSize {
				nextPageToken = encodePageToken(time.Unix(0, lastUpdated).UTC(), a2a.TaskID(lastID))
				break
			}
			var encoded string
			if scanErr := rows.Scan(&encoded, &lastUpdated, &lastID); scanErr != nil {
				return fmt.Errorf("scan a listed task: %w", scanErr)
			}
			task, decodeErr := decodeTask(encoded)
			if decodeErr != nil {
				return decodeErr
			}
			tasks = append(tasks, trimListedTask(task, req))
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			return fmt.Errorf("read the matching tasks: %w", rowsErr)
		}
		response = &a2a.ListTasksResponse{
			Tasks:         tasks,
			TotalSize:     total,
			PageSize:      pageSize,
			NextPageToken: nextPageToken,
		}
		return nil
	})
	switch {
	case errors.Is(err, a2a.ErrParseError):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrTaskStore, err)
	}
	return response, nil
}

// The listing queries.
//
// # Why one static statement instead of a built WHERE clause
//
// Every optional filter is expressed as "this filter is unset, or it matches",
// with the unset test bound as a parameter. It costs one extra placeholder per
// filter and buys a query text that is a compile-time constant: there is no
// code path in this package that can concatenate anything into SQL, so there is
// nothing for a reviewer — or a scanner — to have to verify.
//
// The rows-with-no-timestamp carve-out is the reference implementation's
// behavior, not an accident: the filter narrows what it can judge and stays
// silent about the rest, rather than hiding tasks whose status was never
// stamped.
const (
	taskFilterClause = `
		    owner = ?
		AND (? = '' OR tenant = ?)
		AND (? = '' OR context_id = ?)
		AND (? = '' OR state = ?)
		AND (? = 0 OR status_timestamp_ns IS NULL OR status_timestamp_ns >= ?)`

	countTasksQuery = `SELECT COUNT(*) FROM tasks WHERE` + taskFilterClause

	listTasksQuery = `SELECT task, last_updated_ns, id FROM tasks WHERE` + taskFilterClause + `
		AND (? = 0 OR last_updated_ns < ? OR (last_updated_ns = ? AND id < ?))
		ORDER BY last_updated_ns DESC, id DESC
		LIMIT ?`
)

// taskFilters is one listing request reduced to bound parameters.
type taskFilters struct {
	owner      string
	tenant     string
	contextID  string
	state      string
	cursorID   string
	after      int64
	cursorTime int64
	// hasAfter and hasCursor are flags rather than "is the value zero", because
	// both values have a legitimate zero: the Unix epoch is a valid timestamp
	// and a cursor could name it.
	hasAfter  int
	hasCursor int
}

// listFilters reduces a request to the parameters the two queries bind.
func listFilters(owner string, req *a2a.ListTasksRequest) (taskFilters, error) {
	filters := taskFilters{
		owner:     owner,
		tenant:    req.Tenant,
		contextID: req.ContextID,
		state:     string(req.Status),
	}
	if req.StatusTimestampAfter != nil {
		filters.after, filters.hasAfter = req.StatusTimestampAfter.UnixNano(), 1
	}
	if req.PageToken != "" {
		cursorTime, cursorID, err := decodePageToken(req.PageToken)
		if err != nil {
			return taskFilters{}, err
		}
		filters.cursorTime, filters.cursorID, filters.hasCursor = cursorTime.UnixNano(), string(cursorID), 1
	}
	return filters, nil
}

// match returns the arguments of the shared filter clause.
func (f taskFilters) match() []any {
	return []any{
		f.owner,
		f.tenant, f.tenant,
		f.contextID, f.contextID,
		f.state, f.state,
		f.hasAfter, f.after,
	}
}

// page returns the arguments of the paged listing, cursor and limit included.
func (f taskFilters) page(limit int) []any {
	return append(f.match(), f.hasCursor, f.cursorTime, f.cursorTime, f.cursorID, limit)
}

// trimListedTask applies the per-result history and artifact trimming.
func trimListedTask(task *a2a.Task, req *a2a.ListTasksRequest) *a2a.Task {
	historyLength := defaultMaxHistoryLength
	if req.HistoryLength != nil {
		historyLength = *req.HistoryLength
	}
	switch {
	case historyLength == 0:
		// An empty slice, not nil: the reference distinguishes "history was
		// asked to be empty" from "there is no history field", and so does the
		// JSON it produces.
		task.History = []*a2a.Message{}
	case historyLength > 0 && len(task.History) > historyLength:
		task.History = task.History[len(task.History)-historyLength:]
	}
	if !req.IncludeArtifacts {
		task.Artifacts = nil
	}
	return task
}

// decodeTask reads one stored task document.
func decodeTask(encoded string) (*a2a.Task, error) {
	var task a2a.Task
	if err := json.Unmarshal([]byte(encoded), &task); err != nil {
		return nil, fmt.Errorf("decode a stored task: %w", err)
	}
	return &task, nil
}

// statusTimestamp renders a task's status timestamp for the database.
//
// Unix nanoseconds rather than RFC 3339 text, because the column is both
// compared and ordered: RFC3339Nano trims trailing zeros, so
// "…:00.5Z" sorts before "…:00Z" as a string while being later in time.
func statusTimestamp(task *a2a.Task) any {
	if task.Status.Timestamp == nil || task.Status.Timestamp.IsZero() {
		return nil
	}
	return task.Status.Timestamp.UnixNano()
}

// encodePageToken renders the (last-updated, id) cursor.
//
// The format is byte-for-byte the in-memory store's, so a token minted by one
// implementation is readable by the other — which matters the day a deployment
// switches stores between replicas.
func encodePageToken(updated time.Time, taskID a2a.TaskID) string {
	return base64.URLEncoding.EncodeToString(
		[]byte(fmt.Sprintf("%s_%s", updated.Format(time.RFC3339Nano), taskID)),
	)
}

// decodePageToken reverses [encodePageToken].
//
// It cuts at the first underscore rather than requiring exactly two
// underscore-separated fields, which is the one deliberate difference from the
// reference codec: RFC 3339 never contains an underscore, so the first one is
// always the separator, while the reference's split rejects any task id that
// contains one. Tokens the reference produces still decode here.
func decodePageToken(token string) (time.Time, a2a.TaskID, error) {
	decoded, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("decode the page token: %w", a2a.ErrParseError)
	}
	stamp, id, found := strings.Cut(string(decoded), "_")
	if !found {
		return time.Time{}, "", fmt.Errorf("the page token has no cursor separator: %w", a2a.ErrParseError)
	}
	updated, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("parse the page token timestamp: %w", a2a.ErrParseError)
	}
	return updated, a2a.TaskID(id), nil
}
