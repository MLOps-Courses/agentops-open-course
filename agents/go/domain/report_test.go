package domain

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func triageReportPayload(vocabulary Vocabulary) TriageReportPayload {
	return TriageReportPayload{
		IncidentID:         vocabulary.Incidents.InventoryDown,
		Severity:           string(SeveritySev1),
		AffectedServices:   []string{vocabulary.Services.Inventory, vocabulary.Services.Checkout},
		Hypothesis:         "The upstream dependency stopped answering health checks.",
		Evidence:           []string{"Every replica reports a failed readiness probe."},
		RecommendedRunbook: vocabulary.Runbooks.ServiceDown,
		ProposedActions:    []string{"Restart the failing service after approval."},
	}
}

func TestNewTriageReportAcceptsAModelPayload(t *testing.T) {
	t.Parallel()

	payload := triageReportPayload(Reference())
	report, err := NewTriageReport(payload)
	if err != nil {
		t.Fatalf("NewTriageReport() error = %v, want nil", err)
	}
	if got := report.Payload(); !reportPayloadEqual(got, payload) {
		t.Errorf("Payload() = %+v, want %+v", got, payload)
	}
	if string(report.IncidentID()) != payload.IncidentID {
		t.Errorf("IncidentID() = %q, want %q", report.IncidentID(), payload.IncidentID)
	}
	if report.Severity() != SeveritySev1 {
		t.Errorf("Severity() = %q, want %q", report.Severity(), SeveritySev1)
	}
	if string(report.RecommendedRunbook()) != payload.RecommendedRunbook {
		t.Errorf("RecommendedRunbook() = %q, want %q", report.RecommendedRunbook(), payload.RecommendedRunbook)
	}
	if report.Hypothesis() != payload.Hypothesis {
		t.Errorf("Hypothesis() = %q, want %q", report.Hypothesis(), payload.Hypothesis)
	}
}

// TestTriageReportOmitsNoOptionalField pins the one optional field's default.
// Python's default_factory=list produced [], and downstream automation reading
// null where it expects a list is exactly the silent breakage the schema exists
// to prevent.
func TestTriageReportProposedActionsDefaultToAnEmptyList(t *testing.T) {
	t.Parallel()

	payload := triageReportPayload(Reference())
	payload.ProposedActions = nil

	report, err := NewTriageReport(payload)
	if err != nil {
		t.Fatalf("NewTriageReport() error = %v, want nil", err)
	}
	if got := report.ProposedActions(); len(got) != 0 {
		t.Errorf("ProposedActions() = %v, want empty", got)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v, want nil", err)
	}
	if !strings.Contains(string(encoded), `"proposed_actions":[]`) {
		t.Errorf("encoded report %s does not carry an empty proposed_actions list", encoded)
	}
}

func TestNewTriageReportRejectsAnInvalidPayload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*TriageReportPayload)
		reports []string
	}{
		{
			name:    "incident id is missing",
			mutate:  func(p *TriageReportPayload) { p.IncidentID = "" },
			reports: []string{"incident_id"},
		},
		{
			// Identifiers are parsed, not normalized: a report is a record of
			// what the model concluded, and repairing it would hide drift.
			name:    "incident id is not canonical",
			mutate:  func(p *TriageReportPayload) { p.IncidentID = strings.ToLower(p.IncidentID) },
			reports: []string{"incident_id"},
		},
		{
			name:    "severity is missing",
			mutate:  func(p *TriageReportPayload) { p.Severity = "" },
			reports: []string{"severity"},
		},
		{
			name:    "affected services is empty",
			mutate:  func(p *TriageReportPayload) { p.AffectedServices = nil },
			reports: []string{"affected_services"},
		},
		{
			name:    "an affected service is not a slug",
			mutate:  func(p *TriageReportPayload) { p.AffectedServices[1] = "Upstream" },
			reports: []string{"affected_services[1]"},
		},
		{
			name:    "hypothesis is empty",
			mutate:  func(p *TriageReportPayload) { p.Hypothesis = "" },
			reports: []string{"hypothesis"},
		},
		{
			name:    "evidence is empty",
			mutate:  func(p *TriageReportPayload) { p.Evidence = nil },
			reports: []string{"evidence"},
		},
		{
			name:    "recommended runbook is missing",
			mutate:  func(p *TriageReportPayload) { p.RecommendedRunbook = "" },
			reports: []string{"recommended_runbook"},
		},
		{
			name:    "nothing was filled in",
			mutate:  func(p *TriageReportPayload) { *p = TriageReportPayload{} },
			reports: []string{"incident_id", "severity", "affected_services", "hypothesis", "evidence", "recommended_runbook"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			payload := triageReportPayload(Reference())
			test.mutate(&payload)

			report, err := NewTriageReport(payload)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("NewTriageReport() error = %v, want ErrInvalid", err)
			}
			if len(report.AffectedServices()) != 0 || report.IncidentID() != "" {
				t.Errorf("NewTriageReport() = %+v, want the zero value on failure", report)
			}
			for _, field := range test.reports {
				if !strings.Contains(err.Error(), field+":") {
					t.Errorf("error %q does not name the field %q", err, field)
				}
			}
		})
	}
}

func TestParseTriageReport(t *testing.T) {
	t.Parallel()

	valid, err := json.Marshal(triageReportPayload(Reference()))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v, want nil", err)
	}

	tests := []struct {
		name  string
		data  string
		valid bool
	}{
		{name: "a well formed report", data: string(valid), valid: true},
		// extra="forbid" in Go: a field the schema never declared means the
		// model drifted, and accepting it would let the drift reach
		// downstream automation unnoticed.
		{name: "an undeclared field", data: `{"incident_id":"INC-999","confidence":0.9}`},
		{name: "a truncated document", data: `{"incident_id":`},
		{name: "not an object", data: `"INC-999"`},
		// Only the first document would ever be acted on, so two is a refusal.
		{name: "two documents", data: string(valid) + string(valid)},
		{name: "an empty response", data: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			report, err := ParseTriageReport([]byte(test.data))
			if test.valid {
				if err != nil {
					t.Fatalf("ParseTriageReport() error = %v, want nil", err)
				}
				if report.IncidentID() == "" {
					t.Error("ParseTriageReport() returned an empty report")
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseTriageReport() error = nil, want a failure for %q", test.data)
			}
		})
	}
}

// TestTriageReportAccessorsReturnCopies proves the report a caller holds cannot
// be edited from underneath it.
func TestTriageReportAccessorsReturnCopies(t *testing.T) {
	t.Parallel()

	payload := triageReportPayload(Reference())
	report, err := NewTriageReport(payload)
	if err != nil {
		t.Fatalf("NewTriageReport() error = %v, want nil", err)
	}

	want := report.Payload()
	report.AffectedServices()[0] = "tampered"
	report.Evidence()[0] = "tampered"
	report.ProposedActions()[0] = "tampered"
	// The payload the report was built from is copied in too, so a caller that
	// keeps editing its own slice cannot reach the report either.
	payload.Evidence[0] = "tampered"

	if got := report.Payload(); !reportPayloadEqual(got, want) {
		t.Errorf("Payload() = %+v after tampering, want %+v", got, want)
	}
}

func reportPayloadEqual(a, b TriageReportPayload) bool {
	return a.IncidentID == b.IncidentID &&
		a.Severity == b.Severity &&
		slices.Equal(a.AffectedServices, b.AffectedServices) &&
		a.Hypothesis == b.Hypothesis &&
		slices.Equal(a.Evidence, b.Evidence) &&
		a.RecommendedRunbook == b.RecommendedRunbook &&
		slices.Equal(a.ProposedActions, b.ProposedActions)
}
