package evals

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// ConfirmationTool is the synthetic tool ADK emits instead of running a guarded
// action, carrying the original call inside its arguments. Every approval check
// in this harness keys on this name, because it is the one wire-visible signal
// that a write was proposed and did not happen.
const ConfirmationTool = "adk_request_confirmation"

type ToolCall struct {
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
	CallID string         `json:"call_id,omitempty"`
}

type ToolResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
	CallID   string         `json:"call_id,omitempty"`
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	ModelCalls   int64 `json:"model_calls"`
}

func (u Usage) validate() error {
	for _, field := range []struct {
		name  string
		value int64
	}{
		{"input_tokens", u.InputTokens},
		{"output_tokens", u.OutputTokens},
		{"total_tokens", u.TotalTokens},
		{"model_calls", u.ModelCalls},
	} {
		if field.value < 0 {
			return fmt.Errorf("usage %s must be non-negative", field.name)
		}
	}
	return nil
}

// add sums two usage records and refuses rather than wrapping.
//
// Both operands crossed a provider boundary, so neither is trusted: a negative
// counter or a total that would overflow int64 is a corrupt reading, and a
// corrupt reading that silently becomes a small number is worse than a run that
// stops. Callers turn the error into a failed turn.
func (u Usage) add(other Usage) (Usage, error) {
	if err := errors.Join(u.validate(), other.validate()); err != nil {
		return Usage{}, err
	}
	for _, field := range []struct {
		name        string
		left, right int64
	}{
		{"input_tokens", u.InputTokens, other.InputTokens},
		{"output_tokens", u.OutputTokens, other.OutputTokens},
		{"total_tokens", u.TotalTokens, other.TotalTokens},
		{"model_calls", u.ModelCalls, other.ModelCalls},
	} {
		if field.right > math.MaxInt64-field.left {
			return Usage{}, fmt.Errorf("usage %s totals overflow int64", field.name)
		}
	}
	return Usage{
		InputTokens:  u.InputTokens + other.InputTokens,
		OutputTokens: u.OutputTokens + other.OutputTokens,
		TotalTokens:  u.TotalTokens + other.TotalTokens,
		ModelCalls:   u.ModelCalls + other.ModelCalls,
	}, nil
}

type UsageMetadata struct {
	PromptTokenCount     int64 `json:"promptTokenCount"`
	CandidatesTokenCount int64 `json:"candidatesTokenCount"`
	TotalTokenCount      int64 `json:"totalTokenCount"`
}

func (u *UsageMetadata) Usage() Usage {
	if u == nil {
		return Usage{}
	}
	return Usage{
		InputTokens:  u.PromptTokenCount,
		OutputTokens: u.CandidatesTokenCount,
		TotalTokens:  u.TotalTokenCount,
		ModelCalls:   1,
	}
}

type FunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
	ID   string         `json:"id,omitempty"`
}

type FunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
	ID       string         `json:"id,omitempty"`
}

type Part struct {
	FunctionCall     *FunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *FunctionResponse `json:"functionResponse,omitempty"`
	Text             string            `json:"text,omitempty"`
}

type Content struct {
	Role  string `json:"role,omitempty"`
	Parts []Part `json:"parts,omitempty"`
}

type Event struct {
	Content       *Content       `json:"content"`
	UsageMetadata *UsageMetadata `json:"usageMetadata,omitempty"`
	ID            string         `json:"id,omitempty"`
	InvocationID  string         `json:"invocationId,omitempty"`
	Author        string         `json:"author,omitempty"`
	ErrorCode     string         `json:"errorCode,omitempty"`
	ErrorMessage  string         `json:"errorMessage,omitempty"`
	Partial       bool           `json:"partial,omitempty"`
	TurnComplete  bool           `json:"turnComplete,omitempty"`
}

type Turn struct {
	AwaitingConfirmation *ToolCall      `json:"awaiting_confirmation,omitempty"`
	Text                 string         `json:"text"`
	ErrorCode            string         `json:"error_code,omitempty"`
	ErrorMessage         string         `json:"error_message,omitempty"`
	ToolCalls            []ToolCall     `json:"tool_calls"`
	ToolResponses        []ToolResponse `json:"tool_responses"`
	Events               []Event        `json:"events"`
	Usage                Usage          `json:"usage"`
}

// ToolNames lists the tools the model actually reached for, with ADK's
// confirmation tool filtered out: it is the framework asking a human a question,
// not the agent choosing a capability, and counting it would make every guarded
// proposal look like an extra tool call.
func (t Turn) ToolNames() []string {
	names := make([]string, 0, len(t.ToolCalls))
	for _, call := range t.ToolCalls {
		if call.Name != ConfirmationTool {
			names = append(names, call.Name)
		}
	}
	return names
}

func (t Turn) Failed() bool {
	return t.ErrorCode != ""
}

// Evidence is every tool result this turn saw, encoded as JSON lines.
//
// It is what the groundedness scorer reads: an entity the answer names must
// appear in the question or in here, and "here" is deliberately the tool
// *results* rather than the model's own text, because an answer cannot be its
// own evidence.
func (t Turn) Evidence() string {
	fragments := make([]string, 0, len(t.ToolResponses))
	for _, response := range t.ToolResponses {
		encoded, err := json.Marshal(response.Response)
		if err != nil {
			// Values came through encoding/json, so this can only fail if a caller
			// constructed an invalid Turn. Keep the scorer fail-closed.
			fragments = append(fragments, fmt.Sprintf("<invalid evidence: %v>", err))
			continue
		}
		fragments = append(fragments, string(encoded))
	}
	return strings.Join(fragments, "\n")
}

func FoldEvents(events []Event) Turn {
	turn := Turn{Events: append([]Event(nil), events...)}
	texts := make([]string, 0, len(events))
	for _, event := range events {
		// ADK sends streaming fragments before the final event. Retaining them
		// in Events keeps the capture auditable; folding them again would double
		// text, tool calls, and usage.
		if event.Partial {
			continue
		}
		usage, err := turn.Usage.add(event.UsageMetadata.Usage())
		if err != nil {
			// Usage metadata crosses the provider boundary. Fail the turn closed
			// instead of allowing negative or wrapped counters into evidence.
			turn.ErrorCode = "INVALID_USAGE"
			turn.ErrorMessage = "invalid provider usage metadata"
		} else {
			turn.Usage = usage
		}
		if event.ErrorCode != "" {
			turn.ErrorCode = event.ErrorCode
			turn.ErrorMessage = event.ErrorMessage
		}
		if event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if part.Text != "" {
				texts = append(texts, part.Text)
			}
			if call := part.FunctionCall; call != nil {
				parsed := ToolCall{Name: call.Name, Args: cloneMap(call.Args), CallID: call.ID}
				turn.ToolCalls = append(turn.ToolCalls, parsed)
				if parsed.Name == ConfirmationTool {
					pending := parsed
					turn.AwaitingConfirmation = &pending
				}
			}
			if response := part.FunctionResponse; response != nil {
				turn.ToolResponses = append(turn.ToolResponses, ToolResponse{
					Name:     response.Name,
					Response: cloneMap(response.Response),
					CallID:   response.ID,
				})
				if response.Name == ConfirmationTool {
					turn.AwaitingConfirmation = nil
				}
			}
		}
	}
	turn.Text = strings.TrimSpace(strings.Join(texts, "\n"))
	return turn
}

func cloneMap(source map[string]any) map[string]any {
	if source == nil {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func NewRunID() (string, error) {
	return randomUUID()
}
