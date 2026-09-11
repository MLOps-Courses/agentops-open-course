package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// Notes is the agent's persistent memory service. ADK ships an in-memory
// implementation and a Vertex AI one; a course that promises durable state
// across a restart needs a third, and this is it.
var _ memory.Service = (*Notes)(nil)

// The provenance stamped on every entry, so a consumer can tell a note an
// engineer deliberately wrote from a line of transcript that was ingested.
const (
	incidentNoteSource = "incident_note"
	sessionEventSource = "session_event"
)

// sessionEventTimestampLayout keeps sub-second resolution, unlike the
// second-resolution stamp on a note: a session's events can share a second, and
// AddSessionToMemory has no insertion-order tie-break of its own to fall back on
// once the rows are replaced.
const sessionEventTimestampLayout = time.RFC3339Nano

// AddSessionToMemory ingests one session's text into durable memory.
//
// ADK never calls this for you — no runner, launcher or plugin in the framework
// invokes it — so ingestion stays an explicit act by the host, which is exactly
// the property the bespoke recall tools were built for. The session's previous
// rows are replaced rather than appended to, so adding the same session twice
// is idempotent, matching ADK's own in-memory service.
//
// Only text parts survive. A function call, an inline image or a file reference
// cannot be redacted and cannot be matched, and persisting something that can
// be neither is how a memory store becomes a data-leak surface.
func (n *Notes) AddSessionToMemory(ctx context.Context, current session.Session) (err error) {
	if current == nil {
		return fmt.Errorf("%w: AddSessionToMemory received no session", ErrIncompleteConfig)
	}
	who := scope{appName: current.AppName(), userID: current.UserID()}
	if who.appName == "" {
		who.appName = anonymousScope
	}
	if who.userID == "" {
		who.userID = anonymousScope
	}

	db, err := n.open(ctx, true)
	if err != nil {
		return err
	}
	defer func() { err = closeWith(db, err) }()

	transaction, err := db.BeginTx(ctx, nil)
	if err != nil {
		return notesFailure(notesDatabaseName, fmt.Errorf("begin the ingest transaction: %w", err))
	}
	if _, err := transaction.ExecContext(ctx,
		"DELETE FROM session_memories WHERE app_name = ? AND user_id = ? AND session_id = ?",
		who.appName, who.userID, current.ID(),
	); err != nil {
		return rollback(transaction, notesFailure(notesDatabaseName,
			fmt.Errorf("replace the ingested session: %w", err)))
	}

	events := current.Events()
	if events != nil {
		// The event stream is consumed as an iterator and written row by row: it is
		// never collected into a slice, so a long session costs one row of memory
		// rather than the whole transcript.
		for event := range events.All() {
			if event == nil || event.Content == nil {
				continue
			}
			text := n.redactedText(event.Content)
			if text == "" {
				continue
			}
			if _, err := transaction.ExecContext(ctx,
				`INSERT INTO session_memories
					(app_name, user_id, session_id, event_id, ts, author, role, text)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				who.appName, who.userID, current.ID(), event.ID,
				event.Timestamp.UTC().Format(sessionEventTimestampLayout),
				event.Author, event.Content.Role, text,
			); err != nil {
				return rollback(transaction, notesFailure(notesDatabaseName,
					fmt.Errorf("ingest event %q: %w", event.ID, err)))
			}
		}
	}
	if err := transaction.Commit(); err != nil {
		return rollback(transaction, notesFailure(notesDatabaseName,
			fmt.Errorf("commit the ingested session: %w", err)))
	}
	return nil
}

// redactedText joins one content's text parts, redacting each on the way in.
//
// Redaction happens here rather than at read time for the same reason it does
// for a note: this is the persistence boundary, and durable state is the thing
// being protected.
func (n *Notes) redactedText(content *genai.Content) string {
	parts := make([]string, 0, len(content.Parts))
	for _, part := range content.Parts {
		if part == nil || part.Text == "" {
			continue
		}
		if redacted := strings.TrimSpace(n.redact(part.Text)); redacted != "" {
			parts = append(parts, redacted)
		}
	}
	return strings.Join(parts, "\n")
}

// SearchMemory returns the caller's durable memory for one query.
//
// The recall rule is the one the bespoke tool implements, lifted to ADK's
// interface: the store is scoped to (AppName, UserID), and the query selects an
// incident when it names one — "what did we try on INC-002" recalls that
// incident's notes — and otherwise recalls the most recent notes across every
// incident. Ingested session text is matched separately, by the same
// whole-word intersection ADK's in-memory service uses, so a host that ingests
// sessions gets the framework's semantics for them.
//
// Deliberately curated notes come first: preload_memory injects this list into
// the system instruction verbatim, and what an engineer chose to write down is
// worth more prompt budget than what was said in passing.
func (n *Notes) SearchMemory(
	ctx context.Context, request *memory.SearchRequest,
) (response *memory.SearchResponse, err error) {
	if request == nil {
		return nil, fmt.Errorf("%w: SearchMemory received no request", ErrIncompleteConfig)
	}
	who := scope{appName: request.AppName, userID: request.UserID}
	if who.appName == "" {
		who.appName = anonymousScope
	}
	if who.userID == "" {
		who.userID = anonymousScope
	}

	db, err := n.open(ctx, false)
	if err != nil {
		return nil, err
	}
	defer func() { err = closeWith(db, err) }()

	stored, err := queryNotes(ctx, db, who, incidentIDInQuery(request.Query), recallLimit)
	if err != nil {
		return nil, err
	}
	entries := make([]memory.Entry, 0, len(stored))
	for _, note := range stored {
		entries = append(entries, memory.Entry{
			ID:        "note:" + strconv.FormatInt(note.id, 10),
			Content:   &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: note.note}}},
			Author:    who.userID,
			Timestamp: parseTimestamp(timestampLayout, note.ts),
			CustomMetadata: map[string]any{
				"source":      incidentNoteSource,
				"incident_id": note.incidentID,
			},
		})
	}

	ingested, err := n.searchSessionMemories(ctx, db, who, request.Query)
	if err != nil {
		return nil, err
	}
	return &memory.SearchResponse{Memories: append(entries, ingested...)}, nil
}

// searchSessionMemories returns the ingested transcript lines whose words
// intersect the query's, newest first.
//
// The intersection test and the tokenizer are ADK's: split on spaces,
// lowercase, and match when the two sets share anything at all. It is not a
// good retrieval algorithm, and reproducing it here is deliberate — an agent
// that swaps this service for ADK's in-memory one must not see its recall
// behavior change as a side effect.
func (n *Notes) searchSessionMemories(
	ctx context.Context, db *sql.DB, who scope, query string,
) ([]memory.Entry, error) {
	queryWords := extractWords(query)
	if len(queryWords) == 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT event_id, ts, author, role, text FROM session_memories
		 WHERE app_name = ? AND user_id = ? ORDER BY id DESC`,
		who.appName, who.userID,
	)
	if err != nil {
		return nil, notesFailure(notesDatabaseName, fmt.Errorf("search the ingested sessions: %w", err))
	}
	defer func() { _ = rows.Close() }()

	var entries []memory.Entry
	for rows.Next() && len(entries) < recallLimit {
		var eventID, timestamp, author, role, text string
		if err := rows.Scan(&eventID, &timestamp, &author, &role, &text); err != nil {
			return nil, notesFailure(notesDatabaseName, fmt.Errorf("scan an ingested event: %w", err))
		}
		if !wordsIntersect(extractWords(text), queryWords) {
			continue
		}
		entries = append(entries, memory.Entry{
			ID:             eventID,
			Content:        &genai.Content{Role: role, Parts: []*genai.Part{{Text: text}}},
			Author:         author,
			Timestamp:      parseTimestamp(sessionEventTimestampLayout, timestamp),
			CustomMetadata: map[string]any{"source": sessionEventSource},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, notesFailure(notesDatabaseName, fmt.Errorf("read the ingested sessions: %w", err))
	}
	return entries, nil
}

// incidentIDInQuery returns the incident a free-text query names, or "".
//
// This is what maps ADK's single Query field onto recall_incident_context's
// optional incident filter. A query that names no incident recalls across all
// of them, which is exactly what the tool's empty argument does.
func incidentIDInQuery(query string) string {
	for _, token := range strings.Fields(query) {
		// Punctuation is stripped so "on INC-002," still selects the incident. The
		// identifier grammar admits neither, so nothing valid is lost.
		trimmed := strings.Trim(token, `.,;:!?"'()[]{}<>`)
		if incidentID, err := domain.NormalizeIncidentID(trimmed); err == nil {
			return string(incidentID)
		}
	}
	return ""
}

// extractWords tokenizes text the way ADK's in-memory memory service does:
// split on single spaces, lowercase, drop empties.
func extractWords(text string) map[string]struct{} {
	words := make(map[string]struct{})
	for word := range strings.SplitSeq(text, " ") {
		if word == "" {
			continue
		}
		words[strings.ToLower(word)] = struct{}{}
	}
	return words
}

// wordsIntersect reports whether the two token sets share anything.
func wordsIntersect(left, right map[string]struct{}) bool {
	if len(left) == 0 || len(right) == 0 {
		return false
	}
	// Iterate the smaller set, as ADK does.
	if len(left) > len(right) {
		left, right = right, left
	}
	for word := range left {
		if _, ok := right[word]; ok {
			return true
		}
	}
	return false
}

// parseTimestamp recovers a stored timestamp, falling back to the zero time.
//
// A stamp this package did not write is not worth failing a recall over: the
// entry's text is still the memory, and the zero time is how ADK's own
// formatter says "no time known" (preload_memory omits the Time: line for it).
func parseTimestamp(layout, value string) time.Time {
	parsed, err := time.Parse(layout, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
