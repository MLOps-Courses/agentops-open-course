package evals

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

var (
	evalIDPattern   = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	toolNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

type EvalSet struct {
	ID          string      `json:"eval_set_id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	CreatedAt   json.Number `json:"creation_timestamp,omitempty"`
	Digest      string      `json:"-"`
	Cases       []EvalCase  `json:"eval_cases"`
}

type EvalCase struct {
	SessionInput SessionInput `json:"session_input"`
	ID           string       `json:"eval_id"`
	CreatedAt    json.Number  `json:"creation_timestamp,omitempty"`
	Conversation []Invocation `json:"conversation"`
}

type SessionInput struct {
	State   map[string]any `json:"state"`
	AppName string         `json:"app_name"`
	UserID  string         `json:"user_id"`
}

type Invocation struct {
	Confirmation        *ConfirmationInput  `json:"confirmation,omitempty"`
	ID                  string              `json:"invocation_id"`
	CreatedAt           json.Number         `json:"creation_timestamp,omitempty"`
	IntermediateData    IntermediateData    `json:"intermediate_data"`
	DeterministicChecks DeterministicChecks `json:"deterministic_checks,omitempty"`
	UserContent         EvalContent         `json:"user_content"`
	FinalResponse       EvalContent         `json:"final_response"`
}

// ConfirmationInput turns one conversation entry into a human's answer to the
// approval the previous turn asked for, rather than a new question.
//
// The course's central claim — propose, approve, execute, re-read, report — is a
// conversation, not a turn, and nothing in a single-turn case can score the half
// that happens after a human says yes. A turn that declares this sends the
// ADK confirmation response instead of user text; `user_content` still carries the
// sentence a human would have typed, so the case reads as a transcript.
//
// Approve deliberately has no default: an evalset that means "reject this" must
// say so, because the two produce opposite writes.
type ConfirmationInput struct {
	Approve *bool `json:"approve"`
	// Rationale is what an approver types and what the audit row keeps. The agent
	// refuses an approval without one, so a case that omits it is testing the
	// refusal rather than the loop, and validation says so.
	Rationale string `json:"rationale"`
}

// Approved reports whether this entry approves or rejects.
func (c *ConfirmationInput) Approved() bool {
	return c != nil && c.Approve != nil && *c.Approve
}

type EvalContent struct {
	Role  string     `json:"role"`
	Parts []EvalPart `json:"parts"`
}

type EvalPart struct {
	Text string `json:"text"`
}

type IntermediateData struct {
	ToolUses      []ExpectedToolCall `json:"tool_uses"`
	ToolResponses []any              `json:"intermediate_responses"`
}

type ExpectedToolCall struct {
	Args map[string]any `json:"args"`
	Name string         `json:"name"`
}

type DeterministicChecks struct {
	RequiredConfirmation *ExpectedToolCall `json:"required_confirmation,omitempty"`
	ForbiddenTools       []string          `json:"forbidden_tools,omitempty"`
	ForbiddenOutput      []string          `json:"forbidden_output,omitempty"`
	RequiredOutput       []string          `json:"required_output,omitempty"`
}

func LoadEvalSet(path string) (EvalSet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return EvalSet{}, fmt.Errorf("read evalset %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytesReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	var evalset EvalSet
	if decodeErr := decoder.Decode(&evalset); decodeErr != nil {
		return EvalSet{}, fmt.Errorf("decode evalset %s: %w", path, decodeErr)
	}
	if eofErr := requireJSONEOF(decoder); eofErr != nil {
		return EvalSet{}, fmt.Errorf("decode evalset %s: %w", path, eofErr)
	}
	canonical, err := canonicalJSON(raw)
	if err != nil {
		return EvalSet{}, fmt.Errorf("canonicalize evalset %s: %w", path, err)
	}
	digest := sha256.Sum256(canonical)
	evalset.Digest = hex.EncodeToString(digest[:])
	if err := evalset.Validate(); err != nil {
		return EvalSet{}, fmt.Errorf("validate evalset %s: %w", path, err)
	}
	return evalset, nil
}

// validateConfirmationTurn rejects the three shapes an approval turn cannot have.
//
// Each is a case that would run and score without ever exercising the loop it
// claims to test, which is the failure mode a required case can least afford.
func validateConfirmationTurn(evalCase EvalCase, invocation Invocation, turnIndex int) []error {
	confirmation := invocation.Confirmation
	if confirmation == nil {
		return nil
	}
	var problems []error
	if turnIndex == 0 {
		problems = append(problems, fmt.Errorf(
			"case %q turn 1 answers a confirmation, but nothing has proposed one yet", evalCase.ID,
		))
	}
	if confirmation.Approve == nil {
		problems = append(problems, fmt.Errorf(
			"case %q turn %d must say whether it approves; the two answers write different rows",
			evalCase.ID, turnIndex+1,
		))
	}
	if confirmation.Approved() && strings.TrimSpace(confirmation.Rationale) == "" {
		problems = append(problems, fmt.Errorf(
			"case %q turn %d approves without a rationale, which the agent refuses; the case would score the refusal",
			evalCase.ID, turnIndex+1,
		))
	}
	// A confirmation is answered inside the task that asked for it, so the turn
	// before it must be the one that left a guarded action pending.
	if turnIndex > 0 && evalCase.Conversation[turnIndex-1].DeterministicChecks.RequiredConfirmation == nil {
		problems = append(problems, fmt.Errorf(
			"case %q turn %d answers a confirmation the previous turn does not require",
			evalCase.ID, turnIndex+1,
		))
	}
	return problems
}

func (s EvalSet) Validate() error {
	var problems []error
	if s.ID == "" {
		problems = append(problems, errors.New("eval_set_id is required"))
	}
	if s.Name == "" {
		problems = append(problems, errors.New("name is required"))
	}
	if len(s.Cases) == 0 {
		problems = append(problems, errors.New("at least one eval case is required"))
	}
	seen := make(map[string]struct{}, len(s.Cases))
	for caseIndex, evalCase := range s.Cases {
		if !evalIDPattern.MatchString(evalCase.ID) {
			problems = append(problems, fmt.Errorf("case %d has invalid eval_id %q", caseIndex+1, evalCase.ID))
		}
		if _, exists := seen[evalCase.ID]; exists {
			problems = append(problems, fmt.Errorf("duplicate eval_id %q", evalCase.ID))
		}
		seen[evalCase.ID] = struct{}{}
		if len(evalCase.Conversation) == 0 {
			problems = append(problems, fmt.Errorf("case %q has no conversation", evalCase.ID))
		}
		for turnIndex, invocation := range evalCase.Conversation {
			if invocation.UserContent.Text() == "" {
				problems = append(problems, fmt.Errorf("case %q turn %d has no user text", evalCase.ID, turnIndex+1))
			}
			if invocation.FinalResponse.Text() == "" {
				problems = append(problems, fmt.Errorf("case %q turn %d has no reference response", evalCase.ID, turnIndex+1))
			}
			for toolIndex, tool := range invocation.IntermediateData.ToolUses {
				if tool.Name == "" {
					problems = append(problems, fmt.Errorf(
						"case %q turn %d tool %d has no name",
						evalCase.ID,
						turnIndex+1,
						toolIndex+1,
					))
				}
				if tool.Args == nil {
					problems = append(problems, fmt.Errorf(
						"case %q turn %d tool %q has null args",
						evalCase.ID,
						turnIndex+1,
						tool.Name,
					))
				}
			}
			if err := invocation.DeterministicChecks.Validate(); err != nil {
				problems = append(problems, fmt.Errorf(
					"case %q turn %d deterministic checks: %w",
					evalCase.ID,
					turnIndex+1,
					err,
				))
			}
			checks := invocation.DeterministicChecks
			reference := strings.ToLower(invocation.FinalResponse.Text())
			for _, value := range checks.RequiredOutput {
				if !strings.Contains(reference, strings.ToLower(value)) {
					problems = append(problems, fmt.Errorf(
						"case %q turn %d required output %q is absent from the reviewed reference",
						evalCase.ID,
						turnIndex+1,
						value,
					))
				}
			}
			if required := checks.RequiredConfirmation; required != nil &&
				!expectedToolDeclared(invocation.IntermediateData.ToolUses, *required) {
				problems = append(problems, fmt.Errorf(
					"case %q turn %d required confirmation is absent from the reviewed trajectory",
					evalCase.ID,
					turnIndex+1,
				))
			}
			problems = append(problems, validateConfirmationTurn(evalCase, invocation, turnIndex)...)
		}
	}
	return errors.Join(problems...)
}

func expectedToolDeclared(tools []ExpectedToolCall, required ExpectedToolCall) bool {
	for _, tool := range tools {
		if tool.Name == required.Name && ContainsRequired(tool.Args, required.Args) {
			return true
		}
	}
	return false
}

func (c DeterministicChecks) Validate() error {
	var problems []error
	seenTools := make(map[string]struct{}, len(c.ForbiddenTools))
	for _, name := range c.ForbiddenTools {
		if !toolNamePattern.MatchString(name) {
			problems = append(problems, fmt.Errorf("invalid forbidden tool %q", name))
		}
		if _, exists := seenTools[name]; exists {
			problems = append(problems, fmt.Errorf("duplicate forbidden tool %q", name))
		}
		seenTools[name] = struct{}{}
	}
	seenOutput := make(map[string]struct{}, len(c.ForbiddenOutput))
	for _, value := range c.ForbiddenOutput {
		if value == "" {
			problems = append(problems, errors.New("forbidden output value must not be empty"))
		}
		if _, exists := seenOutput[value]; exists {
			problems = append(problems, fmt.Errorf("duplicate forbidden output value %q", value))
		}
		seenOutput[value] = struct{}{}
	}
	seenRequiredOutput := make(map[string]struct{}, len(c.RequiredOutput))
	for _, value := range c.RequiredOutput {
		if strings.TrimSpace(value) == "" {
			problems = append(problems, errors.New("required output value must not be empty"))
		}
		key := strings.ToLower(value)
		if _, exists := seenRequiredOutput[key]; exists {
			problems = append(problems, fmt.Errorf("duplicate required output value %q", value))
		}
		seenRequiredOutput[key] = struct{}{}
	}
	if required := c.RequiredConfirmation; required != nil {
		if !toolNamePattern.MatchString(required.Name) {
			problems = append(problems, fmt.Errorf("invalid required confirmation tool %q", required.Name))
		}
		if required.Name == ConfirmationTool {
			problems = append(problems, errors.New("required confirmation must name the guarded tool, not the ADK wrapper"))
		}
		if required.Args == nil {
			problems = append(problems, errors.New("required confirmation args must not be null"))
		}
	}
	return errors.Join(problems...)
}

func (s EvalSet) ValidateDomain(domain Domain) error {
	var problems []error
	// The two deliberate negative controls: `unknown-incident` and `unknown-service`
	// ask about an incident and a service the seed data does not hold, to prove the
	// agent says so instead of inventing one. Every other unknown identifier is a
	// typo in a case, and stays an error rather than reaching a model-backed run.
	allowedMissing := map[string]struct{}{"INC-999": {}, "warehouse": {}}
	incidentArgs := map[string]string{
		"get_incident":            "incident_id",
		"recall_incident_context": "incident_id",
		"resolve_incident":        "incident_id",
		"save_incident_note":      "incident_id",
	}
	serviceArgs := map[string]string{
		"get_service_status":  "name",
		"restart_service":     "name",
		"search_service_logs": "service",
	}
	for _, evalCase := range s.Cases {
		for _, invocation := range evalCase.Conversation {
			for _, tool := range invocation.IntermediateData.ToolUses {
				if key, ok := incidentArgs[tool.Name]; ok {
					if value := stringArg(tool.Args, key); value != "" {
						if _, exists := domain.Incidents[value]; !exists {
							if _, negative := allowedMissing[value]; !negative {
								problems = append(problems, fmt.Errorf("case %q references unknown incident %q", evalCase.ID, value))
							}
						}
					}
				}
				if key, ok := serviceArgs[tool.Name]; ok {
					if value := stringArg(tool.Args, key); value != "" {
						if _, exists := domain.Services[value]; !exists {
							if _, negative := allowedMissing[value]; !negative {
								problems = append(problems, fmt.Errorf("case %q references unknown service %q", evalCase.ID, value))
							}
						}
					}
				}
				if tool.Name == "get_runbook" {
					if value := stringArg(tool.Args, "slug"); value != "" {
						if _, exists := domain.Runbooks[value]; !exists {
							problems = append(problems, fmt.Errorf("case %q references unknown runbook %q", evalCase.ID, value))
						}
					}
				}
				if tool.Name == "load_skill" {
					if value := stringArg(tool.Args, "name"); value != "" {
						if _, exists := domain.Skills[value]; !exists {
							problems = append(problems, fmt.Errorf("case %q references unknown skill %q", evalCase.ID, value))
						}
					}
				}
			}
		}
	}
	return errors.Join(problems...)
}

func (c EvalContent) Text() string {
	for _, part := range c.Parts {
		if part.Text != "" {
			return part.Text
		}
	}
	return ""
}

func stringArg(args map[string]any, name string) string {
	value, _ := args[name].(string)
	return value
}

func canonicalJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytesReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
