package policy

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/tools"
)

// This file is the Go port of tests/test_compaction.py.
//
// Every case asserts that the guard returns nil: compaction only ever rewrites
// the outgoing request and must never short-circuit the model call.

// compact runs the guard over a history and hands back the rewritten request.
func compact(t *testing.T, keep *int, history []*genai.Content) []*genai.Content {
	t.Helper()

	policy := newPolicy(t, Config{MaxHistoryMessages: keep})
	request := &model.LLMRequest{Contents: slices.Clone(history)}
	response, err := policy.CompactHistory(newContext(), request)
	if err != nil {
		t.Fatalf("CompactHistory() error = %v, want nil", err)
	}
	if response != nil {
		t.Fatalf("CompactHistory() = %v, want nil: compaction never short-circuits the model call", response)
	}
	return request.Contents
}

// keep returns a pointer to a window size, which is how "off" and "two" are
// told apart in the configuration.
func keep(size int) *int { return &size }

// turns builds a plain conversation of the given length.
func turns(count int) []*genai.Content {
	history := make([]*genai.Content, count)
	for index := range history {
		history[index] = textPart("user", "turn "+string(rune('0'+index)))
	}
	return history
}

func TestCompactionIsDisabledByDefault(t *testing.T) {
	t.Parallel()

	history := turns(10)
	if compacted := compact(t, nil, history); !slices.Equal(compacted, history) {
		t.Errorf("history was rewritten with the knob unset: %d messages became %d",
			len(history), len(compacted))
	}
}

func TestCompactionIsANoOpWithinBudget(t *testing.T) {
	t.Parallel()

	history := turns(5)
	if compacted := compact(t, keep(5), history); !slices.Equal(compacted, history) {
		t.Errorf("history was rewritten while inside the budget: %d messages became %d",
			len(history), len(compacted))
	}
}

func TestCompactionPreservesTheMostRecentMessages(t *testing.T) {
	t.Parallel()

	history := turns(8)
	compacted := compact(t, keep(3), history)

	// One synthetic marker plus the three most recent messages, in order.
	if len(compacted) != 4 {
		t.Fatalf("compacted history has %d messages, want 4", len(compacted))
	}
	if !slices.Equal(compacted[1:], history[len(history)-3:]) {
		t.Error("the retained window is not the three most recent messages")
	}
	marker := compacted[0]
	if marker.Role != markerRole {
		t.Errorf("marker role = %q, want %q so it stays in the frame the model reads", marker.Role, markerRole)
	}
	if note := marker.Parts[0].Text; !strings.HasPrefix(note, "[history compacted: 5 earlier message(s)") {
		t.Errorf("marker = %q, want it to open with the elided count", note)
	}
}

// TestMarkerListsElidedToolNames keeps the elided span legible: the model is
// told which tools it already used, so it does not repeat them.
func TestMarkerListsElidedToolNames(t *testing.T) {
	t.Parallel()

	compacted := compact(t, keep(2), []*genai.Content{
		textPart("user", "diagnose the incident"),
		callPart(tools.GetIncidentToolName, map[string]any{}),
		resultPart(tools.GetIncidentToolName, map[string]any{"ok": true}),
		callPart(tools.SearchServiceLogsToolName, map[string]any{}),
		resultPart(tools.SearchServiceLogsToolName, map[string]any{"ok": true}),
		textPart("user", "what next?"),
	})

	note := compacted[0].Parts[0].Text
	for _, name := range []string{tools.GetIncidentToolName, tools.SearchServiceLogsToolName} {
		if !strings.Contains(note, name) {
			t.Errorf("marker = %q, want it to name %q", note, name)
		}
	}
}

// TestWindowNeverOpensOnAnOrphanToolResult is the first of the two guards on
// the cut: opening the window on a bare tool result would hand the model
// evidence whose matching call it can no longer see.
func TestWindowNeverOpensOnAnOrphanToolResult(t *testing.T) {
	t.Parallel()

	// With a window of three the raw cut lands on the tool result at index 3,
	// whose matching call at index 1 would be dropped.
	compacted := compact(t, keep(3), []*genai.Content{
		textPart("user", "diagnose the incident"),
		callPart(tools.GetIncidentToolName, map[string]any{}),
		textPart("model", "looking"),
		resultPart(tools.GetIncidentToolName, map[string]any{"ok": true}),
		textPart("model", "here is the incident"),
		textPart("user", "what next?"),
	})

	if firstKept := compacted[1]; hasFunctionResponse(firstKept) {
		t.Error("the window opened on a bare tool result, want it advanced to a real message")
	}
	if note := compacted[0].Parts[0].Text; !strings.HasPrefix(note, "[history compacted: 4 earlier message(s)") {
		t.Errorf("marker = %q, want the orphaned result folded into the elided span", note)
	}
}

// TestTrailingFunctionResponsesNeverCollapseToMarkerOnly is the second guard: a
// request can end mid-tool-loop, and replacing the whole of it with a marker
// would discard the evidence the model just asked for.
func TestTrailingFunctionResponsesNeverCollapseToMarkerOnly(t *testing.T) {
	t.Parallel()

	history := []*genai.Content{
		textPart("user", "diagnose the incident"),
		callPart(tools.GetIncidentToolName, map[string]any{}),
		resultPart(tools.GetIncidentToolName, map[string]any{"ok": true}),
		resultPart(tools.SearchServiceLogsToolName, map[string]any{"ok": true}),
		resultPart("get_runbook", map[string]any{"ok": true}),
	}
	compacted := compact(t, keep(2), history)

	if len(compacted) != 2 {
		t.Fatalf("compacted history has %d messages, want 2", len(compacted))
	}
	last := compacted[len(compacted)-1]
	if last != history[len(history)-1] {
		t.Error("the newest tool result was dropped, want it kept")
	}
	if !hasFunctionResponse(last) {
		t.Error("the newest message lost its tool result")
	}
}

// TestNewRejectsAWindowTooSmallToHoldAToolPair keeps a configuration that
// would elide half of every call/result pair from ever reaching the runtime.
func TestNewRejectsAWindowTooSmallToHoldAToolPair(t *testing.T) {
	t.Parallel()

	if _, err := New(Config{MaxHistoryMessages: keep(1)}); err == nil {
		t.Error("New() error = nil for a window of one, want a configuration failure")
	}
}

// TestCompactionIsEphemeral proves the rewrite does not accumulate: ADK rebuilds
// the request from stored session events every turn, so compacting an
// already-compacted history must still produce exactly one marker.
func TestCompactionIsEphemeral(t *testing.T) {
	t.Parallel()

	policy := newPolicy(t, Config{MaxHistoryMessages: keep(3)})
	request := &model.LLMRequest{Contents: turns(8)}
	for range 3 {
		if _, err := policy.CompactHistory(newContext(), request); err != nil {
			t.Fatalf("CompactHistory() error = %v, want nil", err)
		}
	}
	markers := 0
	for _, content := range request.Contents {
		if strings.HasPrefix(content.Parts[0].Text, "[history compacted:") {
			markers++
		}
	}
	if markers != 1 {
		t.Errorf("the request carries %d markers after three passes, want 1", markers)
	}
}
