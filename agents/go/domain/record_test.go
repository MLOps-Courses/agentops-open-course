package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The row builders below take a [Vocabulary] so every record test runs against
// whichever domain it is handed, which is what the portability suite needs.

func serviceRow(vocabulary Vocabulary) ServiceRow {
	return ServiceRow{
		Name:        vocabulary.Services.Inventory,
		Description: "Stock levels and reservations.",
		Status:      string(ServiceStatusDown),
		Owner:       "platform-team",
	}
}

func incidentRow(vocabulary Vocabulary) IncidentRow {
	return IncidentRow{
		ID:         vocabulary.Incidents.InventoryDown,
		Service:    vocabulary.Services.Inventory,
		Title:      "Service is unreachable",
		Severity:   string(SeveritySev1),
		Status:     string(IncidentStatusOpen),
		Runbook:    vocabulary.Runbooks.ServiceDown,
		OpenedAt:   "2026-07-05T09:15:00Z",
		ResolvedAt: nil,
		Summary:    "Health checks fail on every replica.",
	}
}

func auditEntryRow(vocabulary Vocabulary) AuditEntryRow {
	return AuditEntryRow{
		Ts:             "2026-07-05T09:20:00Z",
		Actor:          "agent",
		ApprovedBy:     "reviewer",
		Rationale:      "restore the read path",
		ContextSummary: "one open incident",
		SessionID:      "session-1",
		InvocationID:   "invocation-1",
		Action:         "restart_service",
		Target:         vocabulary.Services.Inventory,
		Detail:         "service restarted and marked operational (mock)",
		ID:             1,
		SchemaVersion:  CurrentAuditSchemaVersion,
	}
}

func TestNewServiceAcceptsATrustedRow(t *testing.T) {
	t.Parallel()

	row := serviceRow(Reference())
	service, err := NewService(row)
	if err != nil {
		t.Fatalf("NewService() error = %v, want nil", err)
	}
	if got := string(service.Name()); got != row.Name {
		t.Errorf("Name() = %q, want %q", got, row.Name)
	}
	if service.Description() != row.Description {
		t.Errorf("Description() = %q, want %q", service.Description(), row.Description)
	}
	if service.Status() != ServiceStatusDown {
		t.Errorf("Status() = %q, want %q", service.Status(), ServiceStatusDown)
	}
	if service.Owner() != row.Owner {
		t.Errorf("Owner() = %q, want %q", service.Owner(), row.Owner)
	}
	if got := service.Row(); got != row {
		t.Errorf("Row() = %+v, want %+v", got, row)
	}
}

func TestNewServiceRejectsAnUntrustedRow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*ServiceRow)
		reports []string
	}{
		{
			name:    "name is not a slug",
			mutate:  func(row *ServiceRow) { row.Name = "Upstream" },
			reports: []string{"name"},
		},
		{
			name:    "name would traverse the filesystem",
			mutate:  func(row *ServiceRow) { row.Name = "../etc" },
			reports: []string{"name"},
		},
		{
			name:    "description is empty",
			mutate:  func(row *ServiceRow) { row.Description = "" },
			reports: []string{"description"},
		},
		{
			name:    "status is unknown",
			mutate:  func(row *ServiceRow) { row.Status = "unhealthy" },
			reports: []string{"status"},
		},
		{
			name:    "owner is empty",
			mutate:  func(row *ServiceRow) { row.Owner = "" },
			reports: []string{"owner"},
		},
		{
			// Every field problem is reported at once so a corrupt seed can be
			// fixed in one pass instead of one round trip per column.
			name: "every field is wrong",
			mutate: func(row *ServiceRow) {
				*row = ServiceRow{}
			},
			reports: []string{"name", "description", "status", "owner"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			row := serviceRow(Reference())
			test.mutate(&row)

			service, err := NewService(row)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("NewService() error = %v, want ErrInvalid", err)
			}
			if service != (Service{}) {
				t.Errorf("NewService() = %+v, want the zero value on failure", service)
			}
			for _, field := range test.reports {
				if !strings.Contains(err.Error(), field+":") {
					t.Errorf("error %q does not name the field %q", err, field)
				}
			}
		})
	}
}

func TestNewIncidentAcceptsATrustedRow(t *testing.T) {
	t.Parallel()

	resolvedAt := "2026-07-05T10:00:00Z"
	tests := []struct {
		name       string
		resolvedAt *string
		want       string
		wantSet    bool
	}{
		{name: "still open", resolvedAt: nil},
		{name: "resolved", resolvedAt: &resolvedAt, want: resolvedAt, wantSet: true},
		// The column carries no constraint at all, so an empty string is a
		// legal value that is distinct from NULL.
		{name: "empty but present", resolvedAt: new(string), wantSet: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			row := incidentRow(Reference())
			row.ResolvedAt = test.resolvedAt

			incident, err := NewIncident(row)
			if err != nil {
				t.Fatalf("NewIncident() error = %v, want nil", err)
			}
			got, set := incident.ResolvedAt()
			if set != test.wantSet || got != test.want {
				t.Errorf("ResolvedAt() = (%q, %t), want (%q, %t)", got, set, test.want, test.wantSet)
			}
			if string(incident.ID()) != row.ID || string(incident.Service()) != row.Service {
				t.Errorf("ID()/Service() = %q/%q, want %q/%q",
					incident.ID(), incident.Service(), row.ID, row.Service)
			}
			if incident.Severity() != SeveritySev1 || incident.Status() != IncidentStatusOpen {
				t.Errorf("Severity()/Status() = %q/%q, want %q/%q",
					incident.Severity(), incident.Status(), SeveritySev1, IncidentStatusOpen)
			}
			if string(incident.Runbook()) != row.Runbook {
				t.Errorf("Runbook() = %q, want %q", incident.Runbook(), row.Runbook)
			}
			if incident.Title() != row.Title || incident.Summary() != row.Summary {
				t.Errorf("Title()/Summary() = %q/%q, want %q/%q",
					incident.Title(), incident.Summary(), row.Title, row.Summary)
			}
			if incident.OpenedAt() != row.OpenedAt {
				t.Errorf("OpenedAt() = %q, want %q", incident.OpenedAt(), row.OpenedAt)
			}
		})
	}
}

func TestIncidentRowReturnsAnUnsharedResolvedAt(t *testing.T) {
	t.Parallel()

	resolvedAt := "2026-07-05T10:00:00Z"
	row := incidentRow(Reference())
	row.ResolvedAt = &resolvedAt

	incident, err := NewIncident(row)
	if err != nil {
		t.Fatalf("NewIncident() error = %v, want nil", err)
	}
	*incident.Row().ResolvedAt = "tampered"
	if got, _ := incident.ResolvedAt(); got != resolvedAt {
		t.Errorf("ResolvedAt() = %q after writing through Row(), want %q", got, resolvedAt)
	}
}

func TestNewIncidentRejectsAnUntrustedRow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*IncidentRow)
		reports []string
	}{
		{name: "id is not an incident", mutate: func(r *IncidentRow) { r.ID = "INCIDENT-2" }, reports: []string{"id"}},
		{name: "service is not a slug", mutate: func(r *IncidentRow) { r.Service = "Warehouse" }, reports: []string{"service"}},
		{name: "title is empty", mutate: func(r *IncidentRow) { r.Title = "" }, reports: []string{"title"}},
		{name: "severity is unknown", mutate: func(r *IncidentRow) { r.Severity = "SEV0" }, reports: []string{"severity"}},
		{name: "status is unknown", mutate: func(r *IncidentRow) { r.Status = "closed" }, reports: []string{"status"}},
		{name: "runbook is not a slug", mutate: func(r *IncidentRow) { r.Runbook = "runbooks/x.md" }, reports: []string{"runbook"}},
		{name: "opened_at is empty", mutate: func(r *IncidentRow) { r.OpenedAt = "" }, reports: []string{"opened_at"}},
		{name: "summary is empty", mutate: func(r *IncidentRow) { r.Summary = "" }, reports: []string{"summary"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			row := incidentRow(Reference())
			test.mutate(&row)

			incident, err := NewIncident(row)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("NewIncident() error = %v, want ErrInvalid", err)
			}
			if incident != (Incident{}) {
				t.Errorf("NewIncident() = %+v, want the zero value on failure", incident)
			}
			for _, field := range test.reports {
				if !strings.Contains(err.Error(), field+":") {
					t.Errorf("error %q does not name the field %q", err, field)
				}
			}
		})
	}
}

func TestNewAuditEntry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*AuditEntryRow)
		reports []string
	}{
		{name: "current schema", mutate: func(*AuditEntryRow) {}},
		{
			// Only rationale and schema_version are constrained; the audit
			// trail stores whatever context it was handed for the rest.
			name: "unconstrained fields accept empty strings",
			mutate: func(row *AuditEntryRow) {
				row.Ts, row.Actor, row.ApprovedBy = "", "", ""
				row.ContextSummary, row.SessionID, row.InvocationID = "", "", ""
				row.Action, row.Target, row.Detail = "", "", ""
			},
		},
		{
			name:   "rationale at the limit",
			mutate: func(row *AuditEntryRow) { row.Rationale = strings.Repeat("a", MaxAuditRationaleLength) },
		},
		{
			// The limit counts characters, not bytes, because that is what the
			// Python model measured and what a reviewer perceives.
			name:   "multi-byte rationale at the limit",
			mutate: func(row *AuditEntryRow) { row.Rationale = strings.Repeat("é", MaxAuditRationaleLength) },
		},
		{
			name:    "rationale is empty",
			mutate:  func(row *AuditEntryRow) { row.Rationale = "" },
			reports: []string{"rationale"},
		},
		{
			name:    "rationale exceeds the limit",
			mutate:  func(row *AuditEntryRow) { row.Rationale = strings.Repeat("a", MaxAuditRationaleLength+1) },
			reports: []string{"rationale"},
		},
		{
			name:    "schema version is unset",
			mutate:  func(row *AuditEntryRow) { row.SchemaVersion = 0 },
			reports: []string{"schema_version"},
		},
		{
			// A row from a newer binary must be refused rather than read with
			// this binary's assumptions.
			name:    "schema version is from the future",
			mutate:  func(row *AuditEntryRow) { row.SchemaVersion = CurrentAuditSchemaVersion + 1 },
			reports: []string{"schema_version"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			row := auditEntryRow(Reference())
			test.mutate(&row)

			entry, err := NewAuditEntry(row)
			if len(test.reports) == 0 {
				if err != nil {
					t.Fatalf("NewAuditEntry() error = %v, want nil", err)
				}
				if got := entry.Row(); got != row {
					t.Errorf("Row() = %+v, want %+v", got, row)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("NewAuditEntry() error = %v, want ErrInvalid", err)
			}
			if entry != (AuditEntry{}) {
				t.Errorf("NewAuditEntry() = %+v, want the zero value on failure", entry)
			}
			for _, field := range test.reports {
				if !strings.Contains(err.Error(), field+":") {
					t.Errorf("error %q does not name the field %q", err, field)
				}
			}
		})
	}
}

func TestAuditEntryAccessorsMirrorTheRow(t *testing.T) {
	t.Parallel()

	row := auditEntryRow(Reference())
	entry, err := NewAuditEntry(row)
	if err != nil {
		t.Fatalf("NewAuditEntry() error = %v, want nil", err)
	}
	got := AuditEntryRow{
		Ts:             entry.Ts(),
		Actor:          entry.Actor(),
		ApprovedBy:     entry.ApprovedBy(),
		Rationale:      entry.Rationale(),
		ContextSummary: entry.ContextSummary(),
		SessionID:      entry.SessionID(),
		InvocationID:   entry.InvocationID(),
		Action:         entry.Action(),
		Target:         entry.Target(),
		Detail:         entry.Detail(),
		ID:             entry.ID(),
		SchemaVersion:  entry.SchemaVersion(),
	}
	if got != row {
		t.Errorf("accessors = %+v, want %+v", got, row)
	}
}

// TestRecordsMarshalThroughTheirRowShape proves the JSON a tool returns is the
// row shape, key for key, and that it parses straight back into a trusted
// value. That round trip is the contract the MCP and A2A surfaces depend on.
func TestRecordsMarshalThroughTheirRowShape(t *testing.T) {
	t.Parallel()

	vocabulary := Reference()

	t.Run("service", func(t *testing.T) {
		t.Parallel()

		want := serviceRow(vocabulary)
		service, err := NewService(want)
		if err != nil {
			t.Fatalf("NewService() error = %v, want nil", err)
		}
		encoded, err := json.Marshal(service)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v, want nil", err)
		}
		var got ServiceRow
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatalf("json.Unmarshal() error = %v, want nil", err)
		}
		if got != want {
			t.Errorf("round trip = %+v, want %+v", got, want)
		}
	})

	t.Run("incident", func(t *testing.T) {
		t.Parallel()

		want := incidentRow(vocabulary)
		incident, err := NewIncident(want)
		if err != nil {
			t.Fatalf("NewIncident() error = %v, want nil", err)
		}
		encoded, err := json.Marshal(incident)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v, want nil", err)
		}
		// A NULL column has to survive as JSON null, not disappear: the read
		// tools report "not resolved yet" by its absence of a timestamp.
		if !strings.Contains(string(encoded), `"resolved_at":null`) {
			t.Errorf("encoded incident %s omits a null resolved_at", encoded)
		}
		var got IncidentRow
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatalf("json.Unmarshal() error = %v, want nil", err)
		}
		if got.ResolvedAt != nil {
			t.Errorf("round trip resolved_at = %q, want nil", *got.ResolvedAt)
		}
		got.ResolvedAt, want.ResolvedAt = nil, nil
		if got != want {
			t.Errorf("round trip = %+v, want %+v", got, want)
		}
	})

	t.Run("audit entry", func(t *testing.T) {
		t.Parallel()

		want := auditEntryRow(vocabulary)
		entry, err := NewAuditEntry(want)
		if err != nil {
			t.Fatalf("NewAuditEntry() error = %v, want nil", err)
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v, want nil", err)
		}
		var got AuditEntryRow
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatalf("json.Unmarshal() error = %v, want nil", err)
		}
		if got != want {
			t.Errorf("round trip = %+v, want %+v", got, want)
		}
	})
}
