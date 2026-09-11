package data

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// RunbooksDir returns the directory holding the runbook knowledge base.
func (s *Store) RunbooksDir() string { return filepath.Join(s.dataDir, "runbooks") }

// LogsDir returns the directory holding the deterministic sample service logs.
func (s *Store) LogsDir() string { return filepath.Join(s.dataDir, "logs") }

// RunbookPath returns the file backing one runbook.
//
// It takes a [domain.Slug] rather than a string, which is the compiler
// enforcing what the Python docstring could only ask for: the slug must already
// have been validated, because this is where a path gets built from it. A slug
// admits neither "/" nor ".", so traversal cannot be expressed.
func (s *Store) RunbookPath(slug domain.Slug) string {
	return filepath.Join(s.RunbooksDir(), string(slug)+".md")
}

// logPath returns the file backing one service's logs.
func (s *Store) logPath(service domain.Slug) string {
	return filepath.Join(s.LogsDir(), string(service)+".log")
}

// ListRunbookSlugs returns every runbook slug in the knowledge base, sorted.
func (s *Store) ListRunbookSlugs() ([]string, error) {
	entries, err := os.ReadDir(s.RunbooksDir())
	if errors.Is(err, fs.ErrNotExist) {
		// An absent knowledge base is a legal, if unhelpful, state: Python's
		// Path.glob yielded nothing rather than raising, and the callers use the
		// empty list to tell the model there is nothing to read.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: could not list the runbook knowledge base %s: %w",
			ErrDataAccess, s.RunbooksDir(), err)
	}
	slugs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if name := entry.Name(); strings.HasSuffix(name, ".md") {
			slugs = append(slugs, strings.TrimSuffix(name, ".md"))
		}
	}
	slices.Sort(slugs)
	return slugs, nil
}

// ReadRunbook returns the full markdown of a runbook, and whether it exists.
//
// The slug is model-controlled, so it is normalized and validated here, before
// anything touches the filesystem: a malformed or traversal-shaped slug is
// reported as "not found" rather than read. That is why this one takes a plain
// string while [Store.RunbookPath] takes a parsed slug — this is the boundary
// where the parsing happens.
func (s *Store) ReadRunbook(slug string) (string, bool, error) {
	normalized, err := domain.NormalizeSlug(slug)
	if err != nil {
		return "", false, nil
	}
	return readTextFile(s.RunbookPath(normalized))
}

// ReadServiceLogs returns a service's log lines, and whether the log exists.
//
// Only some services have logs; a service without one is not an error, it is an
// absence the tool layer reports as such. The slug is normalized here for the
// same reason as in [Store.ReadRunbook].
func (s *Store) ReadServiceLogs(service string) ([]string, bool, error) {
	normalized, err := domain.NormalizeSlug(service)
	if err != nil {
		return nil, false, nil
	}
	content, found, err := readTextFile(s.logPath(normalized))
	if err != nil || !found {
		return nil, found, err
	}
	return splitLines(content), true, nil
}

// readTextFile reads a UTF-8 file, distinguishing "not there" from "could not
// be read". Python treated a non-regular path as absent through is_file(), and
// a genuine read failure propagated; both are preserved.
func readTextFile(path string) (string, bool, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", false, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("%w: could not read %s: %w", ErrDataAccess, filepath.Base(path), err)
	}
	return string(content), true, nil
}

// splitLines splits text the way Python's str.splitlines() did for this corpus:
// on \n, \r\n and \r, dropping a single trailing terminator rather than
// producing an empty final element.
//
// bufio.Scanner handles \n and \r\n; the lone \r that Python also splits on does
// not occur in the committed logs, and treating it as text keeps the line count
// honest for a file that genuinely contains one.
func splitLines(text string) []string {
	scanner := bufio.NewScanner(strings.NewReader(text))
	// A log line has no useful upper bound in the abstract, so raise the limit
	// well past the default 64 KiB rather than silently truncating one.
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), 4*bufio.MaxScanTokenSize)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		// Reading from a strings.Reader cannot fail, so the only reachable error
		// is a line longer than the buffer. Returning what was read would be a
		// silent truncation, so return the whole text as one line instead.
		return []string{text}
	}
	return lines
}

// IncidentFilter narrows an incident listing.
//
// The zero value matches everything, which is Python's default of two None
// arguments. Both fields are parsed types, so "" is unambiguously "no filter":
// neither an empty status nor an empty slug is a value the domain admits, and
// there is therefore no filter this struct can express but not mean.
type IncidentFilter struct {
	Status  domain.IncidentStatus
	Service domain.Slug
}

// ListIncidents returns incidents newest first, optionally filtered.
func (s *Store) ListIncidents(ctx context.Context, filter IncidentFilter) (incidents []domain.Incident, err error) {
	query := "SELECT * FROM incidents"
	var (
		clauses []string
		args    []any
	)
	if filter.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, string(filter.Status))
	}
	if filter.Service != "" {
		clauses = append(clauses, "service = ?")
		args = append(args, string(filter.Service))
	}
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY opened_at DESC"

	db, err := s.openForRead(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { err = db.closeWith(err) }()

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, db.fail(fmt.Errorf("list incidents: %w", err))
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		return nil, db.fail(fmt.Errorf("read the incident columns: %w", err))
	}
	for rows.Next() {
		row, err := scanIncidentRow(rows, columns)
		if err != nil {
			return nil, db.fail(err)
		}
		incident, err := domain.NewIncident(row)
		if err != nil {
			return nil, fmt.Errorf("%w: incident %q in %s is not valid: %w", ErrDataAccess, row.ID, db.name, err)
		}
		incidents = append(incidents, incident)
	}
	if err := rows.Err(); err != nil {
		return nil, db.fail(fmt.Errorf("read incidents: %w", err))
	}
	return incidents, nil
}

// GetIncident returns one incident, or nil when no row carries that id.
//
// A nil result is the honest "no such row", which is where Python returned
// None. The identifier is a parsed [domain.IncidentID], so an id that is merely
// unknown still reaches the query while one that is malformed cannot be built.
func (s *Store) GetIncident(ctx context.Context, id domain.IncidentID) (incident *domain.Incident, err error) {
	db, err := s.openForRead(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { err = db.closeWith(err) }()

	rows, err := db.QueryContext(ctx, "SELECT * FROM incidents WHERE id = ?", string(id))
	if err != nil {
		return nil, db.fail(fmt.Errorf("get incident %s: %w", id, err))
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		return nil, db.fail(fmt.Errorf("read the incident columns: %w", err))
	}
	if !rows.Next() {
		if rowsErr := rows.Err(); rowsErr != nil {
			return nil, db.fail(fmt.Errorf("read incident %s: %w", id, rowsErr))
		}
		return nil, nil
	}
	row, err := scanIncidentRow(rows, columns)
	if err != nil {
		return nil, db.fail(err)
	}
	parsed, err := domain.NewIncident(row)
	if err != nil {
		return nil, fmt.Errorf("%w: incident %q in %s is not valid: %w", ErrDataAccess, row.ID, db.name, err)
	}
	return &parsed, nil
}

// ListServices returns every watched service with its status and owning team.
func (s *Store) ListServices(ctx context.Context) (services []domain.Service, err error) {
	db, err := s.openForRead(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { err = db.closeWith(err) }()

	rows, err := db.QueryContext(ctx, "SELECT * FROM services ORDER BY name")
	if err != nil {
		return nil, db.fail(fmt.Errorf("list services: %w", err))
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		return nil, db.fail(fmt.Errorf("read the service columns: %w", err))
	}
	for rows.Next() {
		row, err := scanServiceRow(rows, columns)
		if err != nil {
			return nil, db.fail(err)
		}
		service, err := domain.NewService(row)
		if err != nil {
			return nil, fmt.Errorf("%w: service %q in %s is not valid: %w", ErrDataAccess, row.Name, db.name, err)
		}
		services = append(services, service)
	}
	if err := rows.Err(); err != nil {
		return nil, db.fail(fmt.Errorf("read services: %w", err))
	}
	return services, nil
}

// GetService returns one service, or nil when no row carries that name.
func (s *Store) GetService(ctx context.Context, name domain.Slug) (service *domain.Service, err error) {
	db, err := s.openForRead(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { err = db.closeWith(err) }()

	rows, err := db.QueryContext(ctx, "SELECT * FROM services WHERE name = ?", string(name))
	if err != nil {
		return nil, db.fail(fmt.Errorf("get service %s: %w", name, err))
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		return nil, db.fail(fmt.Errorf("read the service columns: %w", err))
	}
	if !rows.Next() {
		if rowsErr := rows.Err(); rowsErr != nil {
			return nil, db.fail(fmt.Errorf("read service %s: %w", name, rowsErr))
		}
		return nil, nil
	}
	row, err := scanServiceRow(rows, columns)
	if err != nil {
		return nil, db.fail(err)
	}
	parsed, err := domain.NewService(row)
	if err != nil {
		return nil, fmt.Errorf("%w: service %q in %s is not valid: %w", ErrDataAccess, row.Name, db.name, err)
	}
	return &parsed, nil
}

// scanIncidentRow binds one SELECT * row of incidents by column name.
func scanIncidentRow(rows *sql.Rows, columns []string) (domain.IncidentRow, error) {
	var (
		row domain.IncidentRow
		// resolved_at is the one nullable column, and the distinction is real:
		// NULL means "still open" while an empty string is a stored value. A
		// **string is not a scan target, so it goes through NullString.
		resolvedAt sql.NullString
	)
	destinations, err := bind(columns, map[string]any{
		"id":          &row.ID,
		"service":     &row.Service,
		"title":       &row.Title,
		"severity":    &row.Severity,
		"status":      &row.Status,
		"runbook":     &row.Runbook,
		"opened_at":   &row.OpenedAt,
		"resolved_at": &resolvedAt,
		"summary":     &row.Summary,
	})
	if err != nil {
		return row, fmt.Errorf("bind the incidents row: %w", err)
	}
	if err := rows.Scan(destinations...); err != nil {
		return row, fmt.Errorf("scan an incidents row: %w", err)
	}
	if resolvedAt.Valid {
		row.ResolvedAt = &resolvedAt.String
	}
	return row, nil
}

// scanServiceRow binds one SELECT * row of services by column name.
func scanServiceRow(rows *sql.Rows, columns []string) (domain.ServiceRow, error) {
	var row domain.ServiceRow
	destinations, err := bind(columns, map[string]any{
		"name":        &row.Name,
		"description": &row.Description,
		"status":      &row.Status,
		"owner":       &row.Owner,
	})
	if err != nil {
		return row, fmt.Errorf("bind the services row: %w", err)
	}
	if err := rows.Scan(destinations...); err != nil {
		return row, fmt.Errorf("scan a services row: %w", err)
	}
	return row, nil
}

// scanAuditRow binds one SELECT * row of audit_log by column name.
func scanAuditRow(rows *sql.Rows, columns []string) (domain.AuditEntryRow, error) {
	var row domain.AuditEntryRow
	destinations, err := bind(columns, map[string]any{
		"id":              &row.ID,
		"schema_version":  &row.SchemaVersion,
		"ts":              &row.Ts,
		"actor":           &row.Actor,
		"approved_by":     &row.ApprovedBy,
		"rationale":       &row.Rationale,
		"context_summary": &row.ContextSummary,
		"session_id":      &row.SessionID,
		"invocation_id":   &row.InvocationID,
		"action":          &row.Action,
		"target":          &row.Target,
		"detail":          &row.Detail,
	})
	if err != nil {
		return row, fmt.Errorf("bind the audit_log row: %w", err)
	}
	if err := rows.Scan(destinations...); err != nil {
		return row, fmt.Errorf("scan an audit_log row: %w", err)
	}
	return row, nil
}
