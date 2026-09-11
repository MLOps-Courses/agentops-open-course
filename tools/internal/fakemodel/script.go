package fakemodel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// A Script drives the fixture through a multi-step tool trajectory instead of a
// single text reply, so the evaluation harness's trajectory assertions can run with
// no model, no weights, and no wait.
//
// It is deliberately keyed off the request rather than off server state. Handler is
// stateless by construction — every branch decides from the parsed input alone — and
// a session map would make two concurrent learners interfere. The client replays the
// whole conversation on every call, so the number of tool results already in `input`
// is exactly how far through the trajectory this request is.
type Script struct {
	Cases []ScriptCase `json:"cases"`
}

// ScriptCase matches one evaluation case and lists the steps it walks. Field order
// is chosen for alignment; read it as id, match, steps, answer.
type ScriptCase struct {
	// ID is documentation for a reader of the file; matching uses Match.
	ID string `json:"id"`
	// Match is a substring of the user's question. The first case whose Match
	// appears in the request's input wins, so order the file most-specific first.
	Match string `json:"match"`
	// Answer is returned once every step has been walked.
	Answer string `json:"answer"`
	// Steps are the tool calls this case walks, in order.
	Steps []ScriptStep `json:"steps"`
}

// ScriptStep is one tool call the fixture asks for. A step with no Tool is a
// malformed script rather than a text reply; Answer owns the text.
type ScriptStep struct {
	Arguments map[string]any `json:"arguments"`
	Tool      string         `json:"tool"`
}

// LoadScript reads and validates a trajectory script.
func LoadScript(path string) (*Script, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fake-model script %s: %w", path, err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var script Script
	if err := decoder.Decode(&script); err != nil {
		return nil, fmt.Errorf("decode fake-model script %s: %w", path, err)
	}
	if err := script.Validate(); err != nil {
		return nil, fmt.Errorf("validate fake-model script %s: %w", path, err)
	}
	return &script, nil
}

// Validate rejects a script that could only fail later, at a request.
func (s *Script) Validate() error {
	if len(s.Cases) == 0 {
		return errors.New("script declares no cases")
	}
	seen := make(map[string]bool, len(s.Cases))
	for _, scriptCase := range s.Cases {
		switch {
		case scriptCase.ID == "":
			return errors.New("every case needs an id")
		case seen[scriptCase.ID]:
			return fmt.Errorf("duplicate case id %q", scriptCase.ID)
		case scriptCase.Match == "":
			return fmt.Errorf("case %q needs a match string", scriptCase.ID)
		case scriptCase.Answer == "":
			return fmt.Errorf("case %q needs an answer", scriptCase.ID)
		}
		seen[scriptCase.ID] = true
		for index, step := range scriptCase.Steps {
			if step.Tool == "" {
				return fmt.Errorf("case %q step %d names no tool", scriptCase.ID, index+1)
			}
		}
	}
	return nil
}

// match finds the case this request belongs to, or nil to fall through to the
// unscripted reply.
func (s *Script) match(input json.RawMessage) *ScriptCase {
	if s == nil {
		return nil
	}
	for index := range s.Cases {
		if bytesContains(input, s.Cases[index].Match) {
			return &s.Cases[index]
		}
	}
	return nil
}

// completedSteps counts the tool results the client has already replayed, which is
// how many steps of the trajectory are behind this request.
func completedSteps(input json.RawMessage) int {
	var value any
	if err := json.Unmarshal(input, &value); err != nil {
		return 0
	}
	count := 0
	var visit func(any)
	visit = func(candidate any) {
		switch typed := candidate.(type) {
		case []any:
			for _, item := range typed {
				visit(item)
			}
		case map[string]any:
			if kind, ok := typed["type"].(string); ok && kind == "function_call_output" {
				count++
			}
			for _, item := range typed {
				visit(item)
			}
		}
	}
	visit(value)
	return count
}

// scriptedOutput returns the ADK Responses output items for this step of the
// trajectory: one function_call while steps remain, then the answer.
func scriptedOutput(scriptCase *ScriptCase, completed int) ([]any, bool) {
	if completed >= len(scriptCase.Steps) {
		return nil, false
	}
	step := scriptCase.Steps[completed]
	arguments := step.Arguments
	if arguments == nil {
		arguments = map[string]any{}
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return nil, false
	}
	return []any{map[string]any{
		"id":        fmt.Sprintf("fc-%s-%d", scriptCase.ID, completed),
		"type":      "function_call",
		"status":    "completed",
		"name":      step.Tool,
		"call_id":   fmt.Sprintf("call-%s-%d", scriptCase.ID, completed),
		"arguments": string(encoded),
	}}, true
}

func bytesContains(input json.RawMessage, needle string) bool {
	return strings.Contains(string(input), needle)
}
