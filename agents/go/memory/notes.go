package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/adk/v2/agent"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/data"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

const (
	// anonymousScope is the identity of a call made outside an ADK session —
	// a script, an operator command, a test. It is a stable value rather than a
	// generated one so those calls can recall what they saved.
	anonymousScope = "direct-call"

	// maxNoteLength bounds one note. Memory is meant to hold a sentence an
	// engineer would actually write down, not a transcript.
	maxNoteLength = 2000

	// recallLimit bounds one recall. The model gets recent context, not a
	// complete history it would then have to summarize inside its own window.
	recallLimit = 20

	// notesBusyTimeout is how long a statement waits for another writer's lock.
	// The store is small and every write is a single row, so contention is
	// brief; waiting beats failing the tool call.
	notesBusyTimeout = 5 * time.Second
)

// timestampLayout renders every timestamp this package persists the way the
// rest of the dataset writes one: ISO-8601 UTC at second resolution with a Z.
const timestampLayout = "2006-01-02T15:04:05Z"

// notesSchema is applied on every open, which is what makes a deleted state
// directory a supported operation rather than a crash. Each statement runs
// separately because database/sql has no executescript.
//
// Scoping is (app_name, user_id). The Python original had no application
// column because it served exactly one app; ADK's memory.Service contract
// carries an AppName on every search, and honoring it here is what lets one
// state directory serve two applications without leaking notes between them.
var notesSchema = []string{
	`CREATE TABLE IF NOT EXISTS incident_notes (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		ts          TEXT NOT NULL,
		app_name    TEXT NOT NULL,
		user_id     TEXT NOT NULL,
		incident_id TEXT NOT NULL,
		note        TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_notes_scope ON incident_notes (app_name, user_id, incident_id)`,
	`CREATE TABLE IF NOT EXISTS session_memories (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		app_name   TEXT NOT NULL,
		user_id    TEXT NOT NULL,
		session_id TEXT NOT NULL,
		event_id   TEXT NOT NULL,
		ts         TEXT NOT NULL,
		author     TEXT NOT NULL,
		role       TEXT NOT NULL,
		text       TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_session_memories_scope ON session_memories (app_name, user_id)`,
}

// ErrEmptyUserID refuses an erasure that names no one.
//
// It is a Go error rather than a model-facing refusal because erasure is not a
// tool: only an operator can reach it, and an operator who typed nothing needs
// a non-zero exit status, not a sentence.
var ErrEmptyUserID = errors.New("refusing to erase memory for an empty user id")

// SaveIncidentNoteArgs are the arguments of the save_incident_note tool.
type SaveIncidentNoteArgs struct {
	IncidentID string `json:"incident_id"`
	Note       string `json:"note"`
}

// SavedNote is the note that was persisted, echoed back after redaction so the
// model sees exactly what durable state now holds.
type SavedNote struct {
	IncidentID string `json:"incident_id"`
	Ts         string `json:"ts"`
	Note       string `json:"note"`
}

// SaveIncidentNoteResult is what save_incident_note returns.
type SaveIncidentNoteResult struct {
	Saved *SavedNote `json:"saved,omitzero"`
	Error string     `json:"error,omitzero"`
}

// RecallIncidentContextArgs are the arguments of the recall_incident_context
// tool. An empty incident id recalls across every incident.
type RecallIncidentContextArgs struct {
	IncidentID string `json:"incident_id,omitempty"`
}

// RecalledNote is one remembered note.
type RecalledNote struct {
	Ts         string `json:"ts"`
	IncidentID string `json:"incident_id"`
	Note       string `json:"note"`
}

// RecallIncidentContextResult is what recall_incident_context returns.
type RecallIncidentContextResult struct {
	Count *int           `json:"count,omitzero"`
	Error string         `json:"error,omitzero"`
	Notes []RecalledNote `json:"notes,omitzero"`
}

// ForgottenMemory reports what an erasure removed.
type ForgottenMemory struct {
	UserID string
	Count  int
}

// scope is the (application, user) pair every read and write is confined to.
//
// Cross-session recall therefore requires a stable user id. The unauthenticated
// A2A adapter's context-bound synthetic id is intentionally not human identity,
// so two anonymous callers share nothing across deployments.
type scope struct {
	appName string
	userID  string
}

// scopeOf reads the caller's identity from the ADK tool context, falling back
// to the anonymous scope for a call made outside a session.
func scopeOf(ctx agent.Context) scope {
	who := scope{appName: anonymousScope, userID: anonymousScope}
	if ctx == nil {
		return who
	}
	if appName := ctx.AppName(); appName != "" {
		who.appName = appName
	}
	if userID := ctx.UserID(); userID != "" {
		who.userID = userID
	}
	return who
}

// notesConfig is what [newNotes] needs. It is unexported because [Config] is
// the only supported way to build the store.
type notesConfig struct {
	store          Store
	redact         Redactor
	stateDir       string
	writesDisabled bool
}

// Notes is the durable, per-application, per-user long-term memory store.
//
// It is also the agent's ADK memory.Service — see service.go. Both faces read
// and write the same SQLite file, which is what makes load_memory and
// recall_incident_context two views of one store rather than two stores.
type Notes struct {
	store  Store
	redact Redactor

	stateDir string

	writesDisabled bool
}

func newNotes(cfg notesConfig) *Notes {
	return &Notes{
		store:          cfg.store,
		redact:         cfg.redact,
		stateDir:       cfg.stateDir,
		writesDisabled: cfg.writesDisabled,
	}
}

// Path returns the long-term memory database inside the disposable state
// directory, creating that directory if it does not exist.
func (n *Notes) Path() (string, error) {
	return stateDatabasePath(n.stateDir, notesDatabaseName)
}

// open prepares the note database and applies its schema.
//
// immediate takes the write lock at BEGIN, which only the multi-statement
// session ingest needs; single-statement reads and writes run in autocommit.
func (n *Notes) open(ctx context.Context, immediate bool) (*sql.DB, error) {
	path, err := n.Path()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", data.ErrDataAccess, err)
	}
	db, err := openStateDatabase(ctx, path, notesBusyTimeout, immediate)
	if err != nil {
		return nil, notesFailure(path, err)
	}
	for _, statement := range notesSchema {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return nil, closeWith(db, notesFailure(path, fmt.Errorf("apply the memory schema: %w", err)))
		}
	}
	return db, nil
}

// notesFailure wraps a driver error at the data boundary.
//
// It reports [data.ErrDataAccess] rather than a sentinel of its own, exactly as
// the Python store raised DataAccessError: every SQLite store in the agent
// surfaces one contract, and the composition's error classifier already treats
// that contract as safe to explain to the model.
func notesFailure(path string, err error) error {
	return fmt.Errorf("%w: Long-term memory operation failed for %s: %w", data.ErrDataAccess, path, err)
}

// runSaveIncidentNote is the save_incident_note handler.
//
// It is deliberately not wrapped in a [Guard]: an automatic retry could
// re-deliver the write across an unknown commit boundary and leave two copies
// of one observation in memory.
func (m *Memory) runSaveIncidentNote(ctx agent.Context, args SaveIncidentNoteArgs) (SaveIncidentNoteResult, error) {
	return m.notes.save(callContext(ctx), scopeOf(ctx), args)
}

// SaveOperatorIncidentNote persists one note under an explicit application and
// user identity without manufacturing an ADK tool context.
//
// This is the narrow operator seam used by the platform backup drill. It still
// travels through the production validation and persisted-data redactor; the
// drill is allowed to name its identity explicitly, but it is not allowed to
// bypass the memory boundary merely because no model turn is involved.
func (m *Memory) SaveOperatorIncidentNote(
	ctx context.Context, appName, userID string, args SaveIncidentNoteArgs,
) (SaveIncidentNoteResult, error) {
	if strings.TrimSpace(appName) == "" || strings.TrimSpace(userID) == "" {
		return SaveIncidentNoteResult{}, errors.New("operator memory identity requires non-empty app and user names")
	}
	return m.notes.save(ctx, scope{appName: appName, userID: userID}, args)
}

// save validates, redacts and persists one note.
//
// The order of the checks is load-bearing. The kill-switch is consulted before
// anything touches the filesystem, so a frozen agent does not create a state
// directory or a database as a side effect of being asked to write.
func (n *Notes) save(
	ctx context.Context, who scope, args SaveIncidentNoteArgs,
) (result SaveIncidentNoteResult, err error) {
	if n.writesDisabled {
		// The reason clause comes from config, so this refusal says exactly what
		// the policy guard and the guarded write tools say about the same switch —
		// including how to clear it, which this one used to omit.
		return SaveIncidentNoteResult{Error: "Refusing to save an incident note: " +
			config.WritesDisabledReason + "."}, nil
	}
	incidentID, err := domain.NormalizeIncidentID(args.IncidentID)
	if err != nil {
		return SaveIncidentNoteResult{Error: invalidIncidentID(args.IncidentID)}, nil
	}
	incident, err := n.store.GetIncident(ctx, incidentID)
	if err != nil {
		return SaveIncidentNoteResult{}, fmt.Errorf("get incident %s: %w", incidentID, err)
	}
	if incident == nil {
		return SaveIncidentNoteResult{Error: fmt.Sprintf(
			"No incident found with id %q; refusing to create orphaned memory.", incidentID,
		)}, nil
	}
	cleaned := strings.TrimSpace(args.Note)
	if cleaned == "" {
		return SaveIncidentNoteResult{Error: "Refusing to save an empty note."}, nil
	}
	// Characters, not bytes: the bound exists to keep a human observation short,
	// and an accented word is not two letters long.
	if length := utf8.RuneCountInString(cleaned); length > maxNoteLength {
		return SaveIncidentNoteResult{Error: fmt.Sprintf(
			"Note is too long (%d chars); keep it under %d.", length, maxNoteLength,
		)}, nil
	}
	// Memory is a persistence boundary: redact before the write, not after the
	// read. A credential that reaches the file is already leaked.
	redacted := n.redact(cleaned)
	timestamp := time.Now().UTC().Format(timestampLayout)

	db, err := n.open(ctx, false)
	if err != nil {
		return SaveIncidentNoteResult{}, err
	}
	defer func() { err = closeWith(db, err) }()

	if _, err := db.ExecContext(ctx,
		"INSERT INTO incident_notes (ts, app_name, user_id, incident_id, note) VALUES (?, ?, ?, ?, ?)",
		timestamp, who.appName, who.userID, string(incidentID), redacted,
	); err != nil {
		return SaveIncidentNoteResult{}, notesFailure(notesDatabaseName, fmt.Errorf("insert the note: %w", err))
	}
	return SaveIncidentNoteResult{Saved: &SavedNote{
		IncidentID: string(incidentID),
		Ts:         timestamp,
		Note:       redacted,
	}}, nil
}

// runRecallIncidentContext is the recall_incident_context handler. The identity
// is read before the guard runs, because a retry must recall for the same
// caller as the first attempt.
func (m *Memory) runRecallIncidentContext(
	ctx agent.Context, args RecallIncidentContextArgs,
) (RecallIncidentContextResult, error) {
	who := scopeOf(ctx)
	var result RecallIncidentContextResult
	err := m.guard(callContext(ctx), RecallIncidentContextToolName, func(guarded context.Context) error {
		var err error
		result, err = m.notes.recall(guarded, who, args.IncidentID)
		return err
	})
	return result, err
}

// recall returns the caller's most recent notes, newest first.
func (n *Notes) recall(
	ctx context.Context, who scope, incidentID string,
) (result RecallIncidentContextResult, err error) {
	filter := ""
	if incidentID != "" {
		normalized, parseErr := domain.NormalizeIncidentID(incidentID)
		if parseErr != nil {
			return RecallIncidentContextResult{Error: invalidIncidentID(incidentID)}, nil
		}
		filter = string(normalized)
	}

	db, err := n.open(ctx, false)
	if err != nil {
		return RecallIncidentContextResult{}, err
	}
	defer func() { err = closeWith(db, err) }()

	stored, err := queryNotes(ctx, db, who, filter, recallLimit)
	if err != nil {
		return RecallIncidentContextResult{}, err
	}
	// Allocated non-nil so an empty recall serializes as [] rather than being
	// omitted: "you saved nothing yet" is an answer.
	notes := make([]RecalledNote, 0, len(stored))
	for _, note := range stored {
		notes = append(notes, RecalledNote{Ts: note.ts, IncidentID: note.incidentID, Note: note.note})
	}
	count := len(notes)
	return RecallIncidentContextResult{Count: &count, Notes: notes}, nil
}

// storedNote is one row of the note table, including the row id the ADK memory
// service needs for a stable entry identifier.
type storedNote struct {
	ts         string
	incidentID string
	note       string
	id         int64
}

// queryNotes reads one scope's most recent notes, optionally filtered to a
// single incident.
//
// It backs both faces of the store — the recall tool and the ADK memory service
// — so the scoping rule exists once. An empty incidentID means "every
// incident", which is what both an omitted tool argument and a query that names
// no incident resolve to.
func queryNotes(
	ctx context.Context, db *sql.DB, who scope, incidentID string, limit int,
) ([]storedNote, error) {
	query := "SELECT id, ts, incident_id, note FROM incident_notes WHERE app_name = ? AND user_id = ?"
	arguments := []any{who.appName, who.userID}
	if incidentID != "" {
		query += " AND incident_id = ?"
		arguments = append(arguments, incidentID)
	}
	// Ordered by id, not by ts: the timestamp has second resolution, so two notes
	// saved in the same second would otherwise order arbitrarily. Insertion order
	// is the only tie-break that is always right.
	query += " ORDER BY id DESC LIMIT ?"
	arguments = append(arguments, limit)

	rows, err := db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, notesFailure(notesDatabaseName, fmt.Errorf("recall notes: %w", err))
	}
	defer func() { _ = rows.Close() }()

	notes := make([]storedNote, 0, limit)
	for rows.Next() {
		var note storedNote
		if err := rows.Scan(&note.id, &note.ts, &note.incidentID, &note.note); err != nil {
			return nil, notesFailure(notesDatabaseName, fmt.Errorf("scan a note: %w", err))
		}
		notes = append(notes, note)
	}
	if err := rows.Err(); err != nil {
		return nil, notesFailure(notesDatabaseName, fmt.Errorf("read the notes: %w", err))
	}
	return notes, nil
}

// ForgetUserMemory erases every long-term note one user contributed
// (right-to-erasure, Chapter 7.6).
//
// The append-only audit log is deliberately untouched: erasure covers the
// personal data a user contributed, not the record of who approved which state
// change, which is retained under a separate legal basis. Erasure also crosses
// every application in the state directory — a subject-access request is about
// a person, not about one deployment.
//
// This is an operator action invoked out of band. It is never registered as an
// agent tool, so the model can save and recall but can never erase.
func (n *Notes) ForgetUserMemory(ctx context.Context, userID string) (forgotten ForgottenMemory, err error) {
	cleaned := strings.TrimSpace(userID)
	if cleaned == "" {
		return ForgottenMemory{}, ErrEmptyUserID
	}

	// immediate, because erasure is the one multi-statement write here. Notes and
	// ingested transcripts are the same person's data under the same legal basis,
	// so a run that deleted one and failed on the other would leave a subject
	// half-erased and report success for the half that landed — the worst possible
	// answer to a right-to-erasure request.
	db, err := n.open(ctx, true)
	if err != nil {
		return ForgottenMemory{}, err
	}
	defer func() { err = closeWith(db, err) }()

	transaction, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ForgottenMemory{}, notesFailure(notesDatabaseName, fmt.Errorf("begin the erasure: %w", err))
	}
	defer func() { _ = transaction.Rollback() }()

	result, err := transaction.ExecContext(ctx, "DELETE FROM incident_notes WHERE user_id = ?", cleaned)
	if err != nil {
		return ForgottenMemory{}, notesFailure(notesDatabaseName, fmt.Errorf("erase the notes: %w", err))
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return ForgottenMemory{}, notesFailure(notesDatabaseName, fmt.Errorf("count the erased notes: %w", err))
	}
	if _, err := transaction.ExecContext(ctx, "DELETE FROM session_memories WHERE user_id = ?", cleaned); err != nil {
		return ForgottenMemory{}, notesFailure(notesDatabaseName, fmt.Errorf("erase the session memories: %w", err))
	}
	if err := transaction.Commit(); err != nil {
		return ForgottenMemory{}, notesFailure(notesDatabaseName, fmt.Errorf("commit the erasure: %w", err))
	}
	return ForgottenMemory{UserID: cleaned, Count: int(deleted)}, nil
}

// invalidIncidentID is the refusal both memory tools return for a malformed id.
// The example comes from the vocabulary, so a pivoted domain teaches its own.
func invalidIncidentID(incidentID string) string {
	return fmt.Sprintf(
		"Invalid incident id %q; expected an id like %s.",
		incidentID, domain.Reference().Incidents.InventoryDown,
	)
}
