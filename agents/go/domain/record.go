package domain

import (
	"encoding/json"
	"errors"
	"fmt"
)

// The types in this file follow one shape, and it is the whole point of the
// package:
//
//   - A <T>Row struct with exported, untrusted, string-typed fields. It is the
//     shape a SQLite row scans into and the shape that crosses the tool
//     boundary as JSON. Anything can build one; nothing trusts one.
//   - A <T> struct with unexported fields, reachable only through New<T>. Its
//     accessors return already-parsed types, so a caller holding one cannot
//     ask whether it is valid — the question does not arise.
//
// That is parse-don't-validate: the constructor is the only door, and it
// returns (T, error). The single hole Go leaves is the zero value, `var s
// Service`, which no keyword can forbid; downstream code uses the zero value
// (or a nil pointer) as the honest "no such row" answer, exactly where Python
// returned None.

// ServiceRow is the untrusted shape of one row of the services table.
type ServiceRow struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Owner       string `json:"owner"`
}

// Service is one service parsed from the trusted dataset.
type Service struct {
	name        Slug
	description string
	status      ServiceStatus
	owner       string
}

// NewService parses a services row. Every field problem is reported, not just
// the first, because a corrupt seed is easier to fix from one complete list.
func NewService(row ServiceRow) (Service, error) {
	var errs []error

	name, err := ParseSlug(row.Name)
	if err != nil {
		errs = append(errs, fmt.Errorf("name: %w", err))
	}
	errs = append(errs, requireNonEmpty("description", row.Description))
	status, err := ParseServiceStatus(row.Status)
	if err != nil {
		errs = append(errs, fmt.Errorf("status: %w", err))
	}
	errs = append(errs, requireNonEmpty("owner", row.Owner))

	if err := errors.Join(errs...); err != nil {
		return Service{}, err
	}
	return Service{
		name:        name,
		description: row.Description,
		status:      status,
		owner:       row.Owner,
	}, nil
}

func (s Service) Name() Slug                   { return s.name }
func (s Service) Description() string          { return s.description }
func (s Service) Status() ServiceStatus        { return s.status }
func (s Service) Owner() string                { return s.owner }
func (s Service) MarshalJSON() ([]byte, error) { return marshalRow(s.Row()) }

// Row returns the wire shape of the service. Marshaling goes through it so
// the JSON keys and their order have exactly one definition.
func (s Service) Row() ServiceRow {
	return ServiceRow{
		Name:        string(s.name),
		Description: s.description,
		Status:      string(s.status),
		Owner:       s.owner,
	}
}

// IncidentRow is the untrusted shape of one row of the incidents table.
// ResolvedAt is a pointer because the column is nullable and the empty string
// is a distinct, legal value there.
type IncidentRow struct {
	ID         string  `json:"id"`
	Service    string  `json:"service"`
	Title      string  `json:"title"`
	Severity   string  `json:"severity"`
	Status     string  `json:"status"`
	Runbook    string  `json:"runbook"`
	OpenedAt   string  `json:"opened_at"`
	ResolvedAt *string `json:"resolved_at"`
	Summary    string  `json:"summary"`
}

// Incident is one incident parsed from the trusted dataset.
//
// Field order here is a memory-layout concern only — the fields are unexported
// and the JSON contract lives on [IncidentRow] — so the bool sits last where
// the fieldalignment analyzer wants it.
type Incident struct {
	id            IncidentID
	service       Slug
	title         string
	severity      Severity
	status        IncidentStatus
	runbook       Slug
	openedAt      string
	resolvedAt    string
	summary       string
	hasResolvedAt bool
}

// NewIncident parses an incidents row.
func NewIncident(row IncidentRow) (Incident, error) {
	var errs []error

	id, err := ParseIncidentID(row.ID)
	if err != nil {
		errs = append(errs, fmt.Errorf("id: %w", err))
	}
	service, err := ParseSlug(row.Service)
	if err != nil {
		errs = append(errs, fmt.Errorf("service: %w", err))
	}
	errs = append(errs, requireNonEmpty("title", row.Title))
	severity, err := ParseSeverity(row.Severity)
	if err != nil {
		errs = append(errs, fmt.Errorf("severity: %w", err))
	}
	status, err := ParseIncidentStatus(row.Status)
	if err != nil {
		errs = append(errs, fmt.Errorf("status: %w", err))
	}
	runbook, err := ParseSlug(row.Runbook)
	if err != nil {
		errs = append(errs, fmt.Errorf("runbook: %w", err))
	}
	errs = append(errs, requireNonEmpty("opened_at", row.OpenedAt))
	errs = append(errs, requireNonEmpty("summary", row.Summary))
	// resolved_at carries no constraint at all: NULL means "still open" and an
	// empty string is a legal timestamp-shaped value the seed may hold.

	if err := errors.Join(errs...); err != nil {
		return Incident{}, err
	}
	incident := Incident{
		id:       id,
		service:  service,
		title:    row.Title,
		severity: severity,
		status:   status,
		runbook:  runbook,
		openedAt: row.OpenedAt,
		summary:  row.Summary,
	}
	if row.ResolvedAt != nil {
		incident.resolvedAt = *row.ResolvedAt
		incident.hasResolvedAt = true
	}
	return incident, nil
}

func (i Incident) ID() IncidentID               { return i.id }
func (i Incident) Service() Slug                { return i.service }
func (i Incident) Title() string                { return i.title }
func (i Incident) Severity() Severity           { return i.severity }
func (i Incident) Status() IncidentStatus       { return i.status }
func (i Incident) Runbook() Slug                { return i.runbook }
func (i Incident) OpenedAt() string             { return i.openedAt }
func (i Incident) Summary() string              { return i.summary }
func (i Incident) MarshalJSON() ([]byte, error) { return marshalRow(i.Row()) }

// ResolvedAt reports the resolution timestamp and whether the column was set.
// It returns a value rather than the stored pointer so a caller cannot reach
// back into the incident and change it.
func (i Incident) ResolvedAt() (string, bool) { return i.resolvedAt, i.hasResolvedAt }

// Row returns the wire shape of the incident.
func (i Incident) Row() IncidentRow {
	row := IncidentRow{
		ID:       string(i.id),
		Service:  string(i.service),
		Title:    i.title,
		Severity: string(i.severity),
		Status:   string(i.status),
		Runbook:  string(i.runbook),
		OpenedAt: i.openedAt,
		Summary:  i.summary,
	}
	if i.hasResolvedAt {
		// A fresh pointer per call; sharing one would hand every caller a
		// writable alias into the value they were told is immutable.
		resolvedAt := i.resolvedAt
		row.ResolvedAt = &resolvedAt
	}
	return row
}

// AuditEntryRow is the untrusted shape of one row of the audit_log table.
//
// The JSON contract is the set of key names, which every consumer indexes by;
// the numeric fields trail the strings only because Go's fieldalignment
// analyzer orders a struct by descending field size.
type AuditEntryRow struct {
	Ts             string `json:"ts"`
	Actor          string `json:"actor"`
	ApprovedBy     string `json:"approved_by"`
	Rationale      string `json:"rationale"`
	ContextSummary string `json:"context_summary"`
	SessionID      string `json:"session_id"`
	InvocationID   string `json:"invocation_id"`
	Action         string `json:"action"`
	Target         string `json:"target"`
	Detail         string `json:"detail"`
	ID             int64  `json:"id"`
	SchemaVersion  int    `json:"schema_version"`
}

// AuditEntry is one persisted record of a confirmed state-changing action.
//
// An auditor reading the append-only trail sees who approved it (ApprovedBy),
// why (Rationale), and the decision context recorded alongside the action
// (ContextSummary).
type AuditEntry struct {
	ts             string
	actor          string
	approvedBy     string
	rationale      string
	contextSummary string
	sessionID      string
	invocationID   string
	action         string
	target         string
	detail         string
	id             int64
	schemaVersion  int
}

// NewAuditEntry parses an audit_log row.
//
// Only two fields carry constraints, and both are deliberate: schema_version
// bounds what this binary may read, and rationale must be present and bounded
// because it is the human justification an auditor relies on. The remaining
// fields are unconstrained strings, exactly as in the Python model.
func NewAuditEntry(row AuditEntryRow) (AuditEntry, error) {
	var errs []error

	if row.SchemaVersion < 1 || row.SchemaVersion > CurrentAuditSchemaVersion {
		errs = append(errs, fmt.Errorf(
			"schema_version: %w: %d is outside the supported range 1..%d",
			ErrInvalid, row.SchemaVersion, CurrentAuditSchemaVersion,
		))
	}
	errs = append(errs, requireNonEmpty("rationale", row.Rationale))
	// Length is counted in characters, not bytes: the bound exists to keep a
	// human justification short, and Python counted code points.
	if runes := len([]rune(row.Rationale)); runes > MaxAuditRationaleLength {
		errs = append(errs, fmt.Errorf(
			"rationale: %w: %d characters exceeds the %d character limit",
			ErrInvalid, runes, MaxAuditRationaleLength,
		))
	}

	if err := errors.Join(errs...); err != nil {
		return AuditEntry{}, err
	}
	return AuditEntry{
		id:             row.ID,
		schemaVersion:  row.SchemaVersion,
		ts:             row.Ts,
		actor:          row.Actor,
		approvedBy:     row.ApprovedBy,
		rationale:      row.Rationale,
		contextSummary: row.ContextSummary,
		sessionID:      row.SessionID,
		invocationID:   row.InvocationID,
		action:         row.Action,
		target:         row.Target,
		detail:         row.Detail,
	}, nil
}

func (a AuditEntry) ID() int64                    { return a.id }
func (a AuditEntry) SchemaVersion() int           { return a.schemaVersion }
func (a AuditEntry) Ts() string                   { return a.ts }
func (a AuditEntry) Actor() string                { return a.actor }
func (a AuditEntry) ApprovedBy() string           { return a.approvedBy }
func (a AuditEntry) Rationale() string            { return a.rationale }
func (a AuditEntry) ContextSummary() string       { return a.contextSummary }
func (a AuditEntry) SessionID() string            { return a.sessionID }
func (a AuditEntry) InvocationID() string         { return a.invocationID }
func (a AuditEntry) Action() string               { return a.action }
func (a AuditEntry) Target() string               { return a.target }
func (a AuditEntry) Detail() string               { return a.detail }
func (a AuditEntry) MarshalJSON() ([]byte, error) { return marshalRow(a.Row()) }

// Row returns the wire shape of the audit entry.
func (a AuditEntry) Row() AuditEntryRow {
	return AuditEntryRow{
		ID:             a.id,
		SchemaVersion:  a.schemaVersion,
		Ts:             a.ts,
		Actor:          a.actor,
		ApprovedBy:     a.approvedBy,
		Rationale:      a.rationale,
		ContextSummary: a.contextSummary,
		SessionID:      a.sessionID,
		InvocationID:   a.invocationID,
		Action:         a.action,
		Target:         a.target,
		Detail:         a.detail,
	}
}

// marshalRow serializes a row shape and gives the failure a name. The error is
// unreachable for these structs — every field is a string, an int, or a
// *string — but swallowing it would hide a future field that is not.
func marshalRow[T any](row T) ([]byte, error) {
	encoded, err := json.Marshal(row)
	if err != nil {
		return nil, fmt.Errorf("marshal %T: %w", row, err)
	}
	return encoded, nil
}
