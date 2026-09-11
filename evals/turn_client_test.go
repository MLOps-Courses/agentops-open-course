package evals

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestFoldEventsExcludesStreamingPartials(t *testing.T) {
	t.Parallel()
	events := logicalEvents()
	turn := FoldEvents(events)
	if turn.Text != "inventory is down" {
		t.Fatalf("Text = %q", turn.Text)
	}
	if got := turn.ToolNames(); !reflect.DeepEqual(got, []string{"get_service_status"}) {
		t.Fatalf("ToolNames = %v", got)
	}
	if turn.Usage != (Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10, ModelCalls: 1}) {
		t.Fatalf("Usage = %+v", turn.Usage)
	}
	if len(turn.Events) != 2 || !turn.Events[0].Partial {
		t.Fatalf("raw Events did not retain partial capture: %+v", turn.Events)
	}
}

func TestFoldEventsTracksProviderFailureAndConfirmation(t *testing.T) {
	t.Parallel()
	confirmation := Event{Content: &Content{Parts: []Part{{FunctionCall: &FunctionCall{
		Name: ConfirmationTool, ID: "call-2", Args: map[string]any{"tool": "restart_service"},
	}}}}}
	turn := FoldEvents([]Event{confirmation, {ErrorCode: "MODEL_ERROR", ErrorMessage: "opaque"}})
	if !turn.Failed() || turn.ErrorCode != "MODEL_ERROR" || turn.AwaitingConfirmation == nil {
		t.Fatalf("unexpected folded failure/confirmation: %+v", turn)
	}
}

func TestFoldEventsRejectsInvalidUsageMetadata(t *testing.T) {
	t.Parallel()

	for name, events := range map[string][]Event{
		"negative": {{UsageMetadata: &UsageMetadata{PromptTokenCount: -1}}},
		"overflow": {
			{UsageMetadata: &UsageMetadata{TotalTokenCount: math.MaxInt64}},
			{UsageMetadata: &UsageMetadata{TotalTokenCount: 1}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			turn := FoldEvents(events)
			if turn.ErrorCode != "INVALID_USAGE" {
				t.Fatalf("ErrorCode = %q, want INVALID_USAGE", turn.ErrorCode)
			}
			if turn.Usage.InputTokens < 0 || turn.Usage.TotalTokens < 0 {
				t.Fatalf("invalid usage escaped folding: %+v", turn.Usage)
			}
		})
	}
}

func TestRESTAndA2ATransportsFoldEquivalentTurns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	restServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.Contains(request.URL.Path, "/sessions"):
			writeTestJSON(t, writer, map[string]any{"id": "session-1"})
		case request.URL.Path == "/run":
			writeTestJSON(t, writer, logicalEvents())
		default:
			http.NotFound(writer, request)
		}
	}))
	defer restServer.Close()
	rest, err := NewRESTClient(RESTClientConfig{BaseURL: restServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	session, err := rest.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restTurn, err := rest.Send(ctx, session, "status")
	if err != nil {
		t.Fatal(err)
	}

	a2aServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var envelope struct {
			Method string `json:"method"`
			ID     string `json:"id"`
		}
		if decodeErr := json.NewDecoder(request.Body).Decode(&envelope); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if envelope.Method != "message/stream" {
			t.Fatalf("method = %q", envelope.Method)
		}
		writeTestA2AStream(t, writer, "2.0", envelope.ID, a2aStreamFrames(logicalA2AResult()))
	}))
	defer a2aServer.Close()
	a2a, err := NewA2AClient(A2AClientConfig{BaseURL: a2aServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	a2aSession, err := a2a.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a2aTurn, err := a2a.Send(ctx, a2aSession, "status")
	if err != nil {
		t.Fatal(err)
	}
	restTurn.Events = nil
	a2aTurn.Events = nil
	if !reflect.DeepEqual(restTurn, a2aTurn) {
		t.Fatalf("normalized turns differ\nREST: %+v\nA2A: %+v", restTurn, a2aTurn)
	}
}

func TestA2AClientRejectsInvalidJSONRPCEnvelope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		jsonRPC    string
		responseID func(string) string
		wantError  string
	}{
		{
			name:       "protocol version",
			jsonRPC:    "1.0",
			responseID: func(requestID string) string { return requestID },
			wantError:  "invalid JSON-RPC version",
		},
		{
			name:       "response id",
			jsonRPC:    "2.0",
			responseID: func(string) string { return "unrelated-request" },
			wantError:  "JSON-RPC id mismatch",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var envelope struct {
					ID string `json:"id"`
				}
				if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
					t.Fatal(err)
				}
				writeTestA2AStream(t, writer, test.jsonRPC, test.responseID(envelope.ID),
					a2aStreamFrames(logicalA2AResult()))
			}))
			defer server.Close()

			client, err := NewA2AClient(A2AClientConfig{BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Send(context.Background(), "session", "status")
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Send() error = %v, want error containing %q", err, test.wantError)
			}
		})
	}
}

func TestRESTStreamingUsesRunSSE(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/run_sse" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		for _, event := range logicalEvents() {
			encoded, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = writer.Write([]byte("data: " + string(encoded) + "\n\n"))
		}
	}))
	defer server.Close()
	client, err := NewRESTClient(RESTClientConfig{BaseURL: server.URL, Streaming: true})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := client.Send(context.Background(), "session", "status")
	if err != nil {
		t.Fatal(err)
	}
	if turn.Usage.TotalTokens != 10 || turn.Text != "inventory is down" {
		t.Fatalf("turn = %+v", turn)
	}
}

func TestClientsRejectCredentialURLs(t *testing.T) {
	t.Parallel()
	if _, err := NewRESTClient(RESTClientConfig{BaseURL: "http://secret@example.test"}); err == nil {
		t.Fatal("REST credential URL accepted")
	}
	if _, err := NewA2AClient(A2AClientConfig{BaseURL: "file:///tmp/socket"}); err == nil {
		t.Fatal("A2A non-HTTP URL accepted")
	}
}

func TestFoldEventsClearsAConfirmationOnceItIsAnswered(t *testing.T) {
	t.Parallel()

	pending := Event{Content: &Content{Parts: []Part{{FunctionCall: &FunctionCall{
		Name: ConfirmationTool, ID: "call-confirm", Args: map[string]any{"tool": "restart_service"},
	}}}}}
	if turn := FoldEvents([]Event{pending}); turn.AwaitingConfirmation == nil {
		t.Fatal("FoldEvents(pending) left no pending confirmation")
	}
	// Once the wrapper's response comes back the turn is no longer waiting on a
	// human, and a scorer that still saw it pending would credit an approval the
	// agent already consumed.
	answered := Event{Content: &Content{Parts: []Part{{FunctionResponse: &FunctionResponse{
		Name: ConfirmationTool, ID: "call-confirm", Response: map[string]any{"confirmed": true},
	}}}}}
	turn := FoldEvents([]Event{pending, answered})
	if turn.AwaitingConfirmation != nil {
		t.Fatalf("AwaitingConfirmation = %+v, want nil after the confirmation was answered", turn.AwaitingConfirmation)
	}
	if len(turn.ToolResponses) != 1 || turn.ToolResponses[0].CallID != "call-confirm" {
		t.Fatalf("ToolResponses = %+v, want the answered confirmation", turn.ToolResponses)
	}
}

func TestTurnEvidenceStaysFailClosedOnUnencodableToolOutput(t *testing.T) {
	t.Parallel()

	// Grounding compares the answer against tool evidence. A response the harness
	// cannot serialize must become a visible non-match rather than empty evidence,
	// which would let any claim pass as unsupported-by-nothing.
	turn := Turn{ToolResponses: []ToolResponse{{Response: map[string]any{"handle": make(chan int)}}}}
	if got := turn.Evidence(); !strings.Contains(got, "invalid evidence") {
		t.Fatalf("Evidence() = %q, want a visible invalid-evidence marker", got)
	}
}

func TestNewRunIDMintsDistinctVersion4Identifiers(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, 16)
	for range 16 {
		runID, err := NewRunID()
		if err != nil {
			t.Fatalf("NewRunID() error = %v", err)
		}
		// Artifacts are keyed by run id, so a collision would overwrite evidence.
		if _, duplicate := seen[runID]; duplicate {
			t.Fatalf("NewRunID() repeated %q", runID)
		}
		seen[runID] = struct{}{}
		if len(runID) != 36 || runID[14] != '4' {
			t.Fatalf("NewRunID() = %q, want a 36-character version 4 UUID", runID)
		}
	}
}

func TestRESTConfirmSendsTheADKFunctionResponse(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/run" {
			t.Fatalf("path = %q, want /run", request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		writeTestJSON(t, writer, []Event{{Content: &Content{Parts: []Part{{Text: "restarted"}}}}})
	}))
	defer server.Close()

	client, err := NewRESTClient(RESTClientConfig{BaseURL: server.URL, AppName: "agent", UserID: "evals"})
	if err != nil {
		t.Fatal(err)
	}
	if _, pendingErr := client.Confirm(t.Context(), "session", Turn{}, true, "rationale"); pendingErr == nil ||
		!strings.Contains(pendingErr.Error(), "not awaiting confirmation") {
		t.Fatalf("Confirm(no pending) error = %v, want a refusal", pendingErr)
	}

	turn, err := client.Confirm(t.Context(), "session", pendingConfirmationTurn(), true, "the runbook approves it")
	if err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if turn.Text != "restarted" {
		t.Fatalf("Text = %q, want the resumed turn", turn.Text)
	}
	// The decision has to reach ADK as a response to the exact pending call, or
	// the agent resumes a different action than the one a human approved.
	response := capturedFunctionResponse(t, captured)
	if response["id"] != "call-confirm" || response["name"] != ConfirmationTool {
		t.Fatalf("function response identity = %+v, want the pending confirmation call", response)
	}
	payload, ok := response["response"].(map[string]any)
	if !ok {
		t.Fatalf("function response payload = %T, want an object", response["response"])
	}
	if payload["confirmed"] != true {
		t.Fatalf("confirmed = %v, want true", payload["confirmed"])
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestA2AConfirmResumesTheSameTask(t *testing.T) {
	t.Parallel()

	var messages []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var envelope struct {
			Params struct {
				Message map[string]any `json:"message"`
			} `json:"params"`
			ID string `json:"id"`
		}
		if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, envelope.Params.Message)
		// input-required is the state a guarded write really leaves behind: the task
		// stays open precisely because it is waiting for the approval the next
		// message carries.
		writeTestA2AStream(t, writer, "2.0", envelope.ID, []map[string]any{
			{"kind": "task", "id": "task-1", "contextId": "session"},
			{"kind": "status-update", "status": map[string]any{"state": "input-required"}},
		})
	}))
	defer server.Close()

	client, err := NewA2AClient(A2AClientConfig{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Confirm(t.Context(), "session", Turn{}, false, "rationale"); err == nil ||
		!strings.Contains(err.Error(), "not awaiting confirmation") {
		t.Fatalf("Confirm(no pending) error = %v, want a refusal", err)
	}
	if _, err := client.Send(t.Context(), "session", "restart inventory"); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if _, err := client.Confirm(t.Context(), "session", pendingConfirmationTurn(), false, "not during peak hours"); err != nil {
		t.Fatalf("Confirm() error = %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if len(messages) != 2 {
		t.Fatalf("sent %d messages, want the request and its confirmation", len(messages))
	}
	if messages[0]["taskId"] != nil {
		t.Fatalf("first message carried taskId %v, want none before a task exists", messages[0]["taskId"])
	}
	// A confirmation that opened a new task would leave the guarded action pending
	// forever in the task it was actually proposed in.
	if messages[1]["taskId"] != "task-1" {
		t.Fatalf("confirmation taskId = %v, want the task the proposal belongs to", messages[1]["taskId"])
	}
	parts, ok := messages[1]["parts"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("confirmation parts = %v, want exactly one data part", messages[1]["parts"])
	}
	part, ok := parts[0].(map[string]any)
	if !ok {
		t.Fatalf("confirmation part = %T, want an object", parts[0])
	}
	metadata, ok := part["metadata"].(map[string]any)
	if !ok || metadata["adk_type"] != "function_response" {
		t.Fatalf("confirmation part metadata = %v, want an ADK function response", part["metadata"])
	}
	data, ok := part["data"].(map[string]any)
	if !ok || data["id"] != "call-confirm" || data["name"] != ConfirmationTool {
		t.Fatalf("confirmation data = %v, want the pending confirmation call", part["data"])
	}
}

// A multi-turn case sends its second question into a task the first question
// already finished. A2A refuses that with "task in a terminal state", so the
// client has to start a fresh task and keep only the context id — which is what
// makes conversation-level cases runnable over this transport at all.
func TestA2AClientStartsAFreshTaskAfterATerminalOne(t *testing.T) {
	t.Parallel()

	for name, state := range map[string]string{
		"completed":              "completed",
		"failed":                 "failed",
		"canceled":               "canceled",
		"rejected":               "rejected",
		"protobuf enum spelling": "TASK_STATE_COMPLETED",
		"no status at all":       "",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var messages []map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var envelope struct {
					Params struct {
						Message map[string]any `json:"message"`
					} `json:"params"`
					ID string `json:"id"`
				}
				if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
					t.Error(err)
					return
				}
				messages = append(messages, envelope.Params.Message)
				frames := []map[string]any{{"kind": "task", "id": "task-1", "contextId": "session"}}
				if state != "" {
					frames = append(frames, map[string]any{
						"kind": "status-update", "status": map[string]any{"state": state},
					})
				}
				writeTestA2AStream(t, writer, "2.0", envelope.ID, frames)
			}))
			defer server.Close()

			client, err := NewA2AClient(A2AClientConfig{BaseURL: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			for _, question := range []string{"save a note about INC-002", "what did I just note?"} {
				if _, err := client.Send(t.Context(), "session", question); err != nil {
					t.Fatalf("Send(%q) error = %v", question, err)
				}
			}
			if len(messages) != 2 {
				t.Fatalf("sent %d messages, want two turns", len(messages))
			}
			if messages[1]["taskId"] != nil {
				t.Fatalf("second turn reused taskId %v after a %s task", messages[1]["taskId"], state)
			}
			// The context is what actually carries the conversation; only the task
			// is per-exchange.
			if messages[1]["contextId"] != "session" {
				t.Fatalf("second turn contextId = %v, want the conversation's", messages[1]["contextId"])
			}
		})
	}
}

func TestEventsFromA2ANormalizeMessagesAndFailedTasks(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		check  func(*testing.T, []Event)
		result a2aResult
	}{
		"message result": {
			result: a2aResult{
				Kind: "message", Metadata: map[string]any{"adk_partial": true},
				Parts: []a2aPart{{Kind: "text", Text: "partial fragment"}},
			},
			check: func(t *testing.T, events []Event) {
				t.Helper()
				if len(events) != 1 || !events[0].Partial {
					t.Fatalf("events = %+v, want one partial event", events)
				}
				if turn := FoldEvents(events); turn.Text != "" {
					t.Fatalf("folded text = %q, want the partial fragment excluded", turn.Text)
				}
			},
		},
		"failed task without a code": {
			result: a2aResult{
				Kind:   "task",
				Status: &a2aStatus{State: "failed", Message: &a2aContainer{Parts: []a2aPart{{Kind: "text", Text: "model refused"}}}},
			},
			check: func(t *testing.T, events []Event) {
				t.Helper()
				turn := FoldEvents(events)
				// A failed task must fold into a failed turn: the runner refuses a case
				// whose transport reported failure rather than scoring the empty answer.
				if !turn.Failed() || turn.ErrorCode != "TASK_FAILED" {
					t.Fatalf("turn = %+v, want a TASK_FAILED turn", turn)
				}
				if events[0].ErrorMessage != "model refused" {
					t.Fatalf("ErrorMessage = %q, want the reported status text", events[0].ErrorMessage)
				}
			},
		},
		"failed task with a reported code": {
			result: a2aResult{
				Kind: "task", Metadata: map[string]any{"adk_error_code": "MODEL_ERROR"},
				Status: &a2aStatus{State: "FAILED"},
			},
			check: func(t *testing.T, events []Event) {
				t.Helper()
				if turn := FoldEvents(events); turn.ErrorCode != "MODEL_ERROR" {
					t.Fatalf("ErrorCode = %q, want the reported code", turn.ErrorCode)
				}
			},
		},
		"failed task that already carries a code": {
			result: a2aResult{
				Kind: "task",
				Artifacts: []a2aContainer{{
					Metadata: map[string]any{"adk_error_code": "SAFETY"},
					Parts:    []a2aPart{{Kind: "text", Text: "blocked"}},
				}},
				Status: &a2aStatus{State: "failed"},
			},
			check: func(t *testing.T, events []Event) {
				t.Helper()
				if len(events) != 1 || events[0].ErrorCode != "SAFETY" {
					t.Fatalf("events = %+v, want the artifact's own error code and no synthetic one", events)
				}
			},
		},
		"defaulted tool payloads": {
			result: a2aResult{Kind: "message", Parts: []a2aPart{
				{
					Kind: "data", Metadata: map[string]any{"adk_type": "function_call", "adk_partial": true},
					Data: map[string]any{"name": "get_incident", "id": "call-1"},
				},
				{
					Kind: "data", Metadata: map[string]any{"adk_type": "function_response"},
					Data: map[string]any{"name": "get_incident", "id": "call-1"},
				},
				{Kind: "file", Text: "ignored"},
			}},
			check: func(t *testing.T, events []Event) {
				t.Helper()
				if len(events) != 1 || !events[0].Partial {
					t.Fatalf("events = %+v, want one event marked partial by its part", events)
				}
				parts := events[0].Content.Parts
				if len(parts) != 2 {
					t.Fatalf("parts = %+v, want the unknown kind dropped", parts)
				}
				// Scorers index into args and responses; a nil map there would panic or
				// silently match, so an absent payload becomes an empty object.
				if parts[0].FunctionCall.Args == nil || len(parts[0].FunctionCall.Args) != 0 {
					t.Fatalf("call args = %v, want an empty object", parts[0].FunctionCall.Args)
				}
				if parts[1].FunctionResponse.Response == nil || len(parts[1].FunctionResponse.Response) != 0 {
					t.Fatalf("response = %v, want an empty object", parts[1].FunctionResponse.Response)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			events, err := eventsFromA2A(test.result)
			if err != nil {
				t.Fatalf("eventsFromA2A() error = %v", err)
			}
			test.check(t, events)
		})
	}
}

func TestEventsFromA2ARefuseUndecodablePayloads(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		want   string
		result a2aResult
	}{
		"usage metadata": {
			result: a2aResult{
				Kind:     "message",
				Metadata: map[string]any{"adk_usage_metadata": map[string]any{"promptTokenCount": "many"}},
			},
			want: "decode A2A usage metadata",
		},
		"function call": {
			result: a2aResult{Kind: "message", Parts: []a2aPart{{
				Kind: "data", Metadata: map[string]any{"adk_type": "function_call"},
				Data: map[string]any{"name": map[string]any{"nested": true}},
			}}},
			want: "decode A2A function call",
		},
		"function response": {
			result: a2aResult{Kind: "message", Parts: []a2aPart{{
				Kind: "data", Metadata: map[string]any{"adk_type": "function_response"},
				Data: map[string]any{"response": "not an object"},
			}}},
			want: "decode A2A function response",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// Provider payloads that do not fit the contract must fail the turn rather
			// than fold into zero usage or an empty tool call that scores as a miss.
			if _, err := eventsFromA2A(test.result); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("eventsFromA2A() error = %v, want one mentioning %q", err, test.want)
			}
		})
	}
}

func pendingConfirmationTurn() Turn {
	return FoldEvents([]Event{{Content: &Content{Parts: []Part{{FunctionCall: &FunctionCall{
		Name: ConfirmationTool, ID: "call-confirm",
		Args: map[string]any{"originalFunctionCall": map[string]any{"name": "restart_service"}},
	}}}}}})
}

func capturedFunctionResponse(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	message, ok := payload["newMessage"].(map[string]any)
	if !ok {
		t.Fatalf("newMessage = %T, want an object", payload["newMessage"])
	}
	parts, ok := message["parts"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("parts = %v, want exactly one part", message["parts"])
	}
	part, ok := parts[0].(map[string]any)
	if !ok {
		t.Fatalf("part = %T, want an object", parts[0])
	}
	response, ok := part["functionResponse"].(map[string]any)
	if !ok {
		t.Fatalf("functionResponse = %T, want an object", part["functionResponse"])
	}
	return response
}

func logicalEvents() []Event {
	return []Event{
		{
			Partial:       true,
			Content:       &Content{Role: "model", Parts: []Part{{Text: "inventory"}}},
			UsageMetadata: &UsageMetadata{PromptTokenCount: 2, CandidatesTokenCount: 1, TotalTokenCount: 3},
		},
		{
			Content: &Content{Role: "model", Parts: []Part{
				{FunctionCall: &FunctionCall{Name: "get_service_status", ID: "call-1", Args: map[string]any{"name": "inventory"}}},
				{FunctionResponse: &FunctionResponse{Name: "get_service_status", ID: "call-1", Response: map[string]any{"status": "down"}}},
				{Text: "inventory is down"},
			}},
			UsageMetadata: &UsageMetadata{PromptTokenCount: 7, CandidatesTokenCount: 3, TotalTokenCount: 10},
		},
	}
}

func logicalA2AResult() map[string]any {
	return map[string]any{
		"kind": "task", "id": "task-1", "contextId": "context-1",
		"artifacts": []any{
			map[string]any{
				"metadata": map[string]any{
					"adk_partial": true,
					"adk_usage_metadata": map[string]any{
						"promptTokenCount": 2, "candidatesTokenCount": 1, "totalTokenCount": 3,
					},
				},
				"parts": []any{map[string]any{"kind": "text", "text": "inventory"}},
			},
			map[string]any{
				"metadata": map[string]any{
					"adk_usage_metadata": map[string]any{
						"promptTokenCount": 7, "candidatesTokenCount": 3, "totalTokenCount": 10,
					},
				},
				"parts": []any{
					map[string]any{
						"kind": "data", "metadata": map[string]any{"adk_type": "function_call"},
						"data": map[string]any{"name": "get_service_status", "id": "call-1", "args": map[string]any{"name": "inventory"}},
					},
					map[string]any{
						"kind": "data", "metadata": map[string]any{"adk_type": "function_response"},
						"data": map[string]any{"name": "get_service_status", "id": "call-1", "response": map[string]any{"status": "down"}},
					},
					map[string]any{"kind": "text", "text": "inventory is down"},
				},
			},
		},
	}
}

// a2aStreamFrames turns a stored task into the frames a message/stream response
// would have carried it in. It is the inverse of what the server does: an opening
// snapshot, one artifact update per artifact with that artifact's metadata moved
// onto the frame — which is where the live server puts it, and why the harness
// streams — and a closing status update.
func a2aStreamFrames(result map[string]any) []map[string]any {
	opening := map[string]any{"kind": "task", "status": map[string]any{"state": "submitted"}}
	for _, key := range []string{"id", "contextId"} {
		if value, found := result[key]; found {
			opening[key] = value
		}
	}
	frames := []map[string]any{opening}
	artifacts, _ := result["artifacts"].([]any)
	for _, entry := range artifacts {
		artifact, _ := entry.(map[string]any)
		frames = append(frames, map[string]any{
			"kind":     "artifact-update",
			"artifact": map[string]any{"parts": artifact["parts"]},
			"metadata": artifact["metadata"],
		})
	}
	closing := map[string]any{"kind": "status-update", "status": map[string]any{"state": "completed"}}
	if status, found := result["status"]; found {
		closing["status"] = status
	}
	if metadata, found := result["metadata"]; found {
		closing["metadata"] = metadata
	}
	return append(frames, closing)
}

func writeTestA2AStream(t *testing.T, writer http.ResponseWriter, jsonRPC, responseID string, frames []map[string]any) {
	t.Helper()
	writer.Header().Set("Content-Type", "text/event-stream")
	for _, frame := range frames {
		encoded, err := json.Marshal(map[string]any{"jsonrpc": jsonRPC, "id": responseID, "result": frame})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(writer, "data: %s\n\n", encoded); err != nil {
			t.Fatal(err)
		}
	}
}

func writeTestJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Fatal(err)
	}
}
