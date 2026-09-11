package evals

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	incidentIDPattern = regexp.MustCompile(`^INC-\d{3}$`)
	slugPattern       = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

type TriageReport struct {
	IncidentID         string   `json:"incident_id"`
	Severity           string   `json:"severity"`
	AffectedServices   []string `json:"affected_services"`
	Hypothesis         string   `json:"hypothesis"`
	Evidence           []string `json:"evidence"`
	RecommendedRunbook string   `json:"recommended_runbook"`
	ProposedActions    []string `json:"proposed_actions"`
}

func ParseTriageReport(text string) (TriageReport, error) {
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(text)))
	decoder.DisallowUnknownFields()
	var report TriageReport
	if err := decoder.Decode(&report); err != nil {
		return TriageReport{}, fmt.Errorf("decode triage report: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return TriageReport{}, fmt.Errorf("decode triage report: %w", err)
	}
	var problems []error
	if !incidentIDPattern.MatchString(report.IncidentID) {
		problems = append(problems, fmt.Errorf("incident_id %q is invalid", report.IncidentID))
	}
	if report.Severity != "SEV1" && report.Severity != "SEV2" && report.Severity != "SEV3" {
		problems = append(problems, fmt.Errorf("severity %q is invalid", report.Severity))
	}
	if len(report.AffectedServices) == 0 {
		problems = append(problems, errors.New("affected_services must not be empty"))
	}
	for index, service := range report.AffectedServices {
		if !slugPattern.MatchString(service) {
			problems = append(problems, fmt.Errorf("affected_services[%d] %q is invalid", index, service))
		}
	}
	if strings.TrimSpace(report.Hypothesis) == "" {
		problems = append(problems, errors.New("hypothesis must not be empty"))
	}
	if len(report.Evidence) == 0 {
		problems = append(problems, errors.New("evidence must not be empty"))
	}
	for index, evidence := range report.Evidence {
		if strings.TrimSpace(evidence) == "" {
			problems = append(problems, fmt.Errorf("evidence[%d] must not be empty", index))
		}
	}
	if !slugPattern.MatchString(report.RecommendedRunbook) {
		problems = append(problems, fmt.Errorf("recommended_runbook %q is invalid", report.RecommendedRunbook))
	}
	for index, action := range report.ProposedActions {
		if strings.TrimSpace(action) == "" {
			problems = append(problems, fmt.Errorf("proposed_actions[%d] must not be empty", index))
		}
	}
	if err := errors.Join(problems...); err != nil {
		return TriageReport{}, err
	}
	return report, nil
}

func SchemaScore(turn Turn) Score {
	if _, err := ParseTriageReport(turn.Text); err != nil {
		return NewBinaryScore("schema", false, err.Error())
	}
	return NewBinaryScore("schema", true, "answer is one strict TriageReport JSON object")
}
