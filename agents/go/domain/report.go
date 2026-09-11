package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
)

// TriageReportPayload is the untrusted JSON shape the model must fill.
//
// The jsonschema tags are the field descriptions the model reads; the agent
// package turns this struct into the LLM agent's output schema, which is why
// the fields are exported and string-typed here while [TriageReport] keeps them
// unexported and parsed.
type TriageReportPayload struct {
	IncidentID         string   `json:"incident_id"        jsonschema:"The triaged incident, e.g. INC-002."`
	Severity           string   `json:"severity"           jsonschema:"Severity copied from the incident record."`
	AffectedServices   []string `json:"affected_services"  jsonschema:"Impacted service slugs, root cause first."`
	Hypothesis         string   `json:"hypothesis"         jsonschema:"Likely root cause in one short paragraph."`
	Evidence           []string `json:"evidence"           jsonschema:"Log lines or tool observations backing the hypothesis."`
	RecommendedRunbook string   `json:"recommended_runbook" jsonschema:"Slug of the runbook to follow."`
	ProposedActions    []string `json:"proposed_actions"   jsonschema:"Ordered next steps; guarded actions (restart/resolve) still need human approval."`
}

// TriageReport is the agent's answer as a schema: downstream automation
// (tickets, dashboards) consumes it directly, so a schema violation has to be
// loud rather than a silently-wrong object.
type TriageReport struct {
	incidentID         IncidentID
	severity           Severity
	affectedServices   []Slug
	hypothesis         string
	evidence           []string
	recommendedRunbook Slug
	proposedActions    []string
}

// NewTriageReport parses a model-supplied payload.
//
// The identifiers are parsed strictly rather than normalized. A report is a
// record of what the model concluded, and quietly repairing "Checkout" into
// "checkout" here would hide a model that is not following the schema.
func NewTriageReport(payload TriageReportPayload) (TriageReport, error) {
	var errs []error

	incidentID, err := ParseIncidentID(payload.IncidentID)
	if err != nil {
		errs = append(errs, fmt.Errorf("incident_id: %w", err))
	}
	severity, err := ParseSeverity(payload.Severity)
	if err != nil {
		errs = append(errs, fmt.Errorf("severity: %w", err))
	}
	if len(payload.AffectedServices) == 0 {
		errs = append(errs, fmt.Errorf("affected_services: %w: must list at least one service", ErrInvalid))
	}
	affectedServices := make([]Slug, 0, len(payload.AffectedServices))
	for i, service := range payload.AffectedServices {
		parsed, parseErr := ParseSlug(service)
		if parseErr != nil {
			errs = append(errs, fmt.Errorf("affected_services[%d]: %w", i, parseErr))
			continue
		}
		affectedServices = append(affectedServices, parsed)
	}
	errs = append(errs, requireNonEmpty("hypothesis", payload.Hypothesis))
	if len(payload.Evidence) == 0 {
		errs = append(errs, fmt.Errorf("evidence: %w: must cite at least one observation", ErrInvalid))
	}
	recommendedRunbook, err := ParseSlug(payload.RecommendedRunbook)
	if err != nil {
		errs = append(errs, fmt.Errorf("recommended_runbook: %w", err))
	}

	if err := errors.Join(errs...); err != nil {
		return TriageReport{}, err
	}
	return TriageReport{
		incidentID:         incidentID,
		severity:           severity,
		affectedServices:   affectedServices,
		hypothesis:         payload.Hypothesis,
		evidence:           slices.Clone(payload.Evidence),
		recommendedRunbook: recommendedRunbook,
		// proposed_actions is the one optional field. Normalizing nil to an
		// empty slice here is what makes it serialize back as [] rather than
		// null, matching the Python default_factory=list.
		proposedActions: append([]string{}, payload.ProposedActions...),
	}, nil
}

// ParseTriageReport decodes and validates a model response in one step.
//
// Unknown fields are rejected, which is the Go equivalent of the Python models'
// extra="forbid": a report carrying a field the schema never declared is a
// model that drifted, and accepting it would let the drift reach downstream
// automation unnoticed.
func ParseTriageReport(data []byte) (TriageReport, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var payload TriageReportPayload
	if err := decoder.Decode(&payload); err != nil {
		return TriageReport{}, fmt.Errorf("decode triage report: %w", err)
	}
	// A second value would mean the model emitted two documents; only the
	// first would ever be acted on, so refuse the whole response.
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return TriageReport{}, fmt.Errorf("decode triage report: %w: trailing content after the report", ErrInvalid)
	}
	return NewTriageReport(payload)
}

func (r TriageReport) IncidentID() IncidentID       { return r.incidentID }
func (r TriageReport) Severity() Severity           { return r.severity }
func (r TriageReport) Hypothesis() string           { return r.hypothesis }
func (r TriageReport) RecommendedRunbook() Slug     { return r.recommendedRunbook }
func (r TriageReport) MarshalJSON() ([]byte, error) { return marshalRow(r.Payload()) }

// AffectedServices returns a copy: handing out the backing slice would let a
// caller edit a report it was told is immutable. The same applies to Evidence
// and ProposedActions.
func (r TriageReport) AffectedServices() []Slug  { return slices.Clone(r.affectedServices) }
func (r TriageReport) Evidence() []string        { return slices.Clone(r.evidence) }
func (r TriageReport) ProposedActions() []string { return slices.Clone(r.proposedActions) }

// Payload returns the wire shape of the report.
func (r TriageReport) Payload() TriageReportPayload {
	affectedServices := make([]string, 0, len(r.affectedServices))
	for _, service := range r.affectedServices {
		affectedServices = append(affectedServices, string(service))
	}
	return TriageReportPayload{
		IncidentID:         string(r.incidentID),
		Severity:           string(r.severity),
		AffectedServices:   affectedServices,
		Hypothesis:         r.hypothesis,
		Evidence:           slices.Clone(r.evidence),
		RecommendedRunbook: string(r.recommendedRunbook),
		ProposedActions:    slices.Clone(r.proposedActions),
	}
}
