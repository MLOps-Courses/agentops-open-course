// Package data is the data-access layer over the bundled Ops Copilot dataset
// (SQLite + runbooks). It is the shared source of truth the tools (Ch. 3.1) build on,
// kept separate so it can be unit-tested directly against agents/data/incidents.db.
package data

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	// Pure-Go SQLite driver (no cgo) so the agent builds into a distroless image (Ch. 6).
	_ "modernc.org/sqlite"
)

// Incident is one row of the incidents table. JSON tags shape the tool output the model sees.
type Incident struct {
	ID         string `json:"id"`
	Service    string `json:"service"`
	Title      string `json:"title"`
	Severity   string `json:"severity"`
	Status     string `json:"status"`
	Runbook    string `json:"runbook"`
	OpenedAt   string `json:"opened_at"`
	ResolvedAt string `json:"resolved_at,omitempty"`
	Summary    string `json:"summary"`
}

// Service is one row of the services table.
type Service struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Owner       string `json:"owner"`
}

// AuditEntry is one appended row of the audit log (mock actions, Ch. 4.5).
type AuditEntry struct {
	ID     int64  `json:"id"`
	TS     string `json:"ts"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail"`
}

// Store owns the database handle and the data directory (for locating runbooks).
type Store struct {
	db  *sql.DB
	dir string
}

// Open connects to incidents.db inside dir and verifies it is reachable.
func Open(dir string) (*Store, error) {
	db, err := sql.Open("sqlite", filepath.Join(dir, "incidents.db"))
	if err != nil {
		return nil, fmt.Errorf("opening dataset in %q: %w", dir, err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("pinging dataset in %q: %w", dir, err)
	}
	return &Store{db: db, dir: dir}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// RunbookPath returns the file path of a runbook by slug (knowledge base, Ch. 3.4).
func (s *Store) RunbookPath(slug string) string {
	return filepath.Join(s.dir, "runbooks", slug+".md")
}

// ListRunbookSlugs returns the slugs of every runbook in the knowledge base, sorted.
func (s *Store) ListRunbookSlugs() ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(s.dir, "runbooks", "*.md"))
	if err != nil {
		return nil, fmt.Errorf("listing runbooks: %w", err)
	}
	slugs := make([]string, 0, len(matches))
	for _, match := range matches {
		slugs = append(slugs, strings.TrimSuffix(filepath.Base(match), ".md"))
	}
	sort.Strings(slugs)
	return slugs, nil
}

// ReadRunbook returns the markdown of a runbook by slug, or ok=false if it does not exist.
func (s *Store) ReadRunbook(slug string) (string, bool, error) {
	content, err := os.ReadFile(s.RunbookPath(slug))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading runbook %q: %w", slug, err)
	}
	return string(content), true, nil
}

func scanIncident(rows *sql.Rows) (Incident, error) {
	var inc Incident
	var resolved sql.NullString
	if err := rows.Scan(&inc.ID, &inc.Service, &inc.Title, &inc.Severity, &inc.Status,
		&inc.Runbook, &inc.OpenedAt, &resolved, &inc.Summary); err != nil {
		return Incident{}, fmt.Errorf("scanning incident: %w", err)
	}
	inc.ResolvedAt = resolved.String
	return inc, nil
}

const incidentColumns = "id, service, title, severity, status, runbook, opened_at, resolved_at, summary"

// A single static query keeps SQL out of string building: an empty filter argument disables
// its clause, and every user value is bound with a placeholder (no injection surface).
const listIncidentsQuery = "SELECT " + incidentColumns + " FROM incidents " +
	"WHERE (? = '' OR status = ?) AND (? = '' OR service = ?) ORDER BY opened_at DESC"

// ListIncidents returns incidents newest first, optionally filtered by status and/or service
// (pass "" to skip a filter).
func (s *Store) ListIncidents(status, service string) ([]Incident, error) {
	rows, err := s.db.Query(listIncidentsQuery, status, status, service, service)
	if err != nil {
		return nil, fmt.Errorf("querying incidents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	incidents := []Incident{}
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		incidents = append(incidents, inc)
	}
	return incidents, rows.Err()
}

// GetIncident returns one incident by id, or ok=false if it does not exist.
func (s *Store) GetIncident(id string) (Incident, bool, error) {
	rows, err := s.db.Query("SELECT "+incidentColumns+" FROM incidents WHERE id = ?", id)
	if err != nil {
		return Incident{}, false, fmt.Errorf("querying incident %q: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return Incident{}, false, rows.Err()
	}
	inc, err := scanIncident(rows)
	if err != nil {
		return Incident{}, false, err
	}
	return inc, true, nil
}

// ListServices returns every watched service, ordered by name.
func (s *Store) ListServices() ([]Service, error) {
	rows, err := s.db.Query("SELECT name, description, status, owner FROM services ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("querying services: %w", err)
	}
	defer func() { _ = rows.Close() }()

	services := []Service{}
	for rows.Next() {
		var svc Service
		if err := rows.Scan(&svc.Name, &svc.Description, &svc.Status, &svc.Owner); err != nil {
			return nil, fmt.Errorf("scanning service: %w", err)
		}
		services = append(services, svc)
	}
	return services, rows.Err()
}

// GetService returns one service by name, or ok=false if it does not exist.
func (s *Store) GetService(name string) (Service, bool, error) {
	var svc Service
	err := s.db.QueryRow("SELECT name, description, status, owner FROM services WHERE name = ?", name).
		Scan(&svc.Name, &svc.Description, &svc.Status, &svc.Owner)
	if errors.Is(err, sql.ErrNoRows) {
		return Service{}, false, nil
	}
	if err != nil {
		return Service{}, false, fmt.Errorf("querying service %q: %w", name, err)
	}
	return svc, true, nil
}

func nowUTC() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }

// SetServiceStatus updates a service's status (mock action); ok reports whether a row changed.
func (s *Store) SetServiceStatus(name, status string) (bool, error) {
	res, err := s.db.Exec("UPDATE services SET status = ? WHERE name = ?", status, name)
	if err != nil {
		return false, fmt.Errorf("updating service %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reading rows affected: %w", err)
	}
	return n > 0, nil
}

// ResolveIncident marks an open incident resolved with a resolved_at timestamp (mock action);
// ok reports whether an unresolved incident was updated.
func (s *Store) ResolveIncident(id string) (bool, error) {
	res, err := s.db.Exec(
		"UPDATE incidents SET status = 'resolved', resolved_at = ? WHERE id = ? AND status != 'resolved'",
		nowUTC(), id,
	)
	if err != nil {
		return false, fmt.Errorf("resolving incident %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reading rows affected: %w", err)
	}
	return n > 0, nil
}

// AppendAudit appends one entry to the audit log and returns it (used by mock actions, Ch. 4.5).
func (s *Store) AppendAudit(actor, action, target, detail string) (AuditEntry, error) {
	ts := nowUTC()
	res, err := s.db.Exec(
		"INSERT INTO audit_log (ts, actor, action, target, detail) VALUES (?, ?, ?, ?, ?)",
		ts, actor, action, target, detail,
	)
	if err != nil {
		return AuditEntry{}, fmt.Errorf("appending audit entry: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return AuditEntry{}, fmt.Errorf("reading audit id: %w", err)
	}
	return AuditEntry{ID: id, TS: ts, Actor: actor, Action: action, Target: target, Detail: detail}, nil
}
