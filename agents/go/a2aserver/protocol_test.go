package a2aserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/a2aserver"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/data"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/policy"
	agenttools "github.com/MLOps-Courses/agentops-open-course/agents/go/tools"
)

// The native a2a-go wire contract, exercised over net/http/httptest against the
// real handler and consumed by the checked-in browser and evaluator clients.

func TestConversationCompletesOverJSONRPC(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	server := fixture.serve(t)

	envelope := rpc(t, server, a2aserver.RootPath, "message/send", textMessage("m1", "Hello."))
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		t.Fatalf("message/send returned %v, want a task", envelope)
	}
	if got := result["kind"]; got != "task" {
		t.Errorf("kind = %v, want %q", got, "task")
	}
	status, _ := result["status"].(map[string]any)
	if got := status["state"]; got != "completed" {
		t.Errorf("state = %v, want %q", got, "completed")
	}
	if got := fixture.model.callCount(); got != 1 {
		t.Errorf("the model was called %d times, want 1", got)
	}
}

func TestStreamingTurnEmitsTheDocumentedEventShape(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	server := fixture.serve(t)

	results := streamResults(t, server, textMessage("m1", "Hello."))
	wantKinds := []string{"task", "status-update", "artifact-update", "status-update"}
	if got := kinds(results); !slices.Equal(got, wantKinds) {
		t.Errorf("event kinds = %v, want %v", got, wantKinds)
	}
	wantStates := []string{"submitted", "working", "completed"}
	if got := states(results); !slices.Equal(got, wantStates) {
		t.Errorf("task states = %v, want %v", got, wantStates)
	}
	last := results[len(results)-1]
	if last["final"] != true {
		t.Errorf("final = %v on the terminal event, want true", last["final"])
	}
	// Every event after the first carries the same task and context ids, which
	// is what lets a client correlate a stream it reconnects to.
	taskID, contextID := results[0]["id"], results[0]["contextId"]
	for index, result := range results[1:] {
		if result["taskId"] != taskID || result["contextId"] != contextID {
			t.Errorf("event %d = %v, want task %v in context %v", index+1, result, taskID, contextID)
		}
	}
}

// TestServerSentEventFramesMatchTheCheckedInBrowserParser is the Python CRLF
// assertion, rewritten around what a2a-go actually writes.
//
// The Python stack framed events with CRLF; a2a-go writes LF. The checked-in
// browser client accepts both — its frame regex is /\r\n\r\n|\n\n|\r\r/ — so
// the client still works, and the honest assertion is that the bytes on the
// wire are covered by the parser that has to read them.
func TestServerSentEventFramesMatchTheCheckedInBrowserParser(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	server := fixture.serve(t)

	response := post(t, server, a2aserver.RootPath, "message/stream", textMessage("m1", "Hello."))
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the event stream: %v", err)
	}
	if !strings.Contains(string(raw), "\n\n") {
		t.Errorf("the stream carries no frame separator: %q", raw)
	}

	client, err := os.ReadFile(filepath.Join("..", "..", "..", "clients", "web", "index.html"))
	if err != nil {
		t.Fatalf("reading the checked-in browser client: %v", err)
	}
	for _, fragment := range []string{
		`/\r\n\r\n|\n\n|\r\r/`, // the frame separator this server actually writes
		`/\r\n|\r|\n/`,         // the line separator inside a frame
		`"tasks/cancel"`,       // the method TestCancellationEndsTheTaskCanceled drives
		`aria-label="Cancel the active task"`,
		// The flag that tells a provisional chunk from the whole redacted
		// message, asserted on the wire by
		// TestStreamedChunksCrossTheRedactionBoundary.
		`meta.adk_partial`,
	} {
		if !strings.Contains(string(client), fragment) {
			t.Errorf("the browser client no longer contains %s", fragment)
		}
	}
}

// TestGuardedActionPausesForConfirmationAndResumes is the HITL round trip the
// deployment contract exists for: a confirmation-gated tool stops the turn, the
// approval arrives as a data part on the same task, and only then does the tool
// run.
func TestGuardedActionPausesForConfirmationAndResumes(t *testing.T) {
	t.Parallel()

	var (
		guard sync.Mutex
		runs  []string
	)
	confirmTool := newConfirmingTool(t, func(name string) {
		guard.Lock()
		defer guard.Unlock()
		runs = append(runs, name)
	})
	model := &scriptedLLM{turns: [][]*adkmodel.LLMResponse{
		toolCallTurn("restart-call", confirmedToolName, map[string]any{"name": "target"}),
		textTurn("The approved restart completed and was audited."),
		textTurn("Nothing further."),
	}}
	fixture := newFixture(t, func(opts *fixtureOptions) {
		opts.model = model
		opts.agent = newAgent(t, model, func(cfg *llmagent.Config) { cfg.Tools = []tool.Tool{confirmTool} })
	})
	server := fixture.serve(t)

	paused := streamResults(t, server, textMessage("m1", "Restart the target."))
	last := paused[len(paused)-1]
	if got := states(paused); got[len(got)-1] != "input-required" {
		t.Fatalf("task states = %v, want the turn to pause", got)
	}
	// The tool must not have run: the pause is the guarantee, not a formality.
	guard.Lock()
	beforeApproval := len(runs)
	guard.Unlock()
	if beforeApproval != 0 {
		t.Fatalf("the guarded tool ran %d times before approval, want 0", beforeApproval)
	}

	call := confirmationCall(t, last)
	original, ok := call["args"].(map[string]any)["originalFunctionCall"].(map[string]any)
	if !ok {
		t.Fatalf("confirmation call = %v, want it to carry the original call", call)
	}
	if original["name"] != confirmedToolName || original["id"] != "restart-call" {
		t.Errorf("original call = %v, want the exact tool call the model made", original)
	}

	resumed := streamResults(t, server, map[string]any{"message": map[string]any{
		"kind":      "message",
		"messageId": "m2",
		"role":      "user",
		"contextId": last["contextId"],
		"taskId":    last["taskId"],
		"parts": []any{map[string]any{
			"kind": "data",
			"data": map[string]any{
				"id":   call["id"],
				"name": call["name"],
				"response": map[string]any{
					"confirmed": true,
					"payload":   map[string]any{"rationale": "The runbook prescribes a restart."},
				},
			},
			"metadata": map[string]any{"adk_type": "function_response"},
		}},
	}})

	final := resumed[len(resumed)-1]
	if final["taskId"] != last["taskId"] || final["contextId"] != last["contextId"] {
		t.Errorf("the resumed turn = %v, want it on the paused task", final)
	}
	if got := states(resumed); got[len(got)-1] != "completed" {
		t.Errorf("resumed states = %v, want the turn to complete", got)
	}
	if final["final"] != true {
		t.Errorf("final = %v on the terminal event, want true", final["final"])
	}
	guard.Lock()
	defer guard.Unlock()
	if !slices.Equal(runs, []string{"target"}) {
		t.Errorf("the guarded tool ran %v, want exactly one approved call", runs)
	}
}

// TestUnauthenticatedA2AConfirmationCannotMutateTheRealStore joins the wire
// confirmation flow to the shipped guarded tool and transactional state store.
// The generic protocol test above proves ADK resumes a confirmed function; this
// test proves that resumption alone grants no network write authority.
func TestUnauthenticatedA2AConfirmationCannotMutateTheRealStore(t *testing.T) {
	t.Parallel()

	inventory := domain.Reference().Services.Inventory
	model := &scriptedLLM{turns: [][]*adkmodel.LLMResponse{
		toolCallTurn("restart-call", agenttools.RestartServiceToolName, map[string]any{"name": inventory}),
		textTurn("The unauthenticated restart was refused."),
	}}
	fixture := newRealRestartFixture(t, model, "")
	server := fixture.serve(t)

	paused := streamResults(t, server, textMessage("m1", "Restart "+inventory+"."))
	last := paused[len(paused)-1]
	if got := states(paused); got[len(got)-1] != "input-required" {
		t.Fatalf("task states = %v, want the turn to pause", got)
	}
	call := confirmationCall(t, last)
	resumed := streamResults(t, server,
		confirmationMessage(last, call, "anonymous confirmation is not authorization"))
	if got := states(resumed); got[len(got)-1] != "completed" {
		t.Fatalf("resumed states = %v, want a completed refusal", got)
	}

	name, err := domain.NormalizeSlug(inventory)
	if err != nil {
		t.Fatalf("NormalizeSlug() error = %v, want nil", err)
	}
	service, err := fixture.store.GetService(t.Context(), name)
	if err != nil || service == nil {
		t.Fatalf("GetService() = %+v, %v, want %s", service, err, inventory)
	}
	if service.Status() != domain.ServiceStatusDown {
		t.Errorf("%s status = %q, want %q", inventory, service.Status(), domain.ServiceStatusDown)
	}
	pool := openRaw(t, filepath.Join(fixture.stateDir, "incidents.db"))
	var auditRows int
	if scanErr := pool.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM audit_log").Scan(&auditRows); scanErr != nil {
		t.Fatalf("count audit rows: %v", scanErr)
	}
	if auditRows != 0 {
		t.Errorf("audit rows = %d, want no record for a refused write", auditRows)
	}
	encoded, err := json.Marshal(model.observedRequests())
	if err != nil {
		t.Fatalf("marshal observed requests: %v", err)
	}
	if !strings.Contains(string(encoded), "network action requires an authenticated principal") {
		t.Errorf("model requests do not contain the authorization refusal: %s", encoded)
	}
}

// TestAuthenticatedA2AConfirmationMutatesAndAuditsTheRealStore proves the
// positive half of the boundary: the subject authenticated on both HTTP turns
// survives ADK's pause/resume flow and owns the committed audit row.
func TestAuthenticatedA2AConfirmationMutatesAndAuditsTheRealStore(t *testing.T) {
	t.Parallel()

	inventory := domain.Reference().Services.Inventory
	model := &scriptedLLM{turns: [][]*adkmodel.LLMResponse{
		toolCallTurn("restart-call", agenttools.RestartServiceToolName, map[string]any{"name": inventory}),
		textTurn("The authenticated restart completed."),
	}}
	fixture := newRealRestartFixture(t, model, identityHeader)
	server := fixture.serve(t)
	authenticated := [2]string{identityHeader, "alice@example.test"}

	paused := streamMethod(t, server, a2aserver.RootPath, "message/stream",
		textMessage("m1", "Restart "+inventory+"."), authenticated)
	last := paused[len(paused)-1]
	if got := states(paused); got[len(got)-1] != "input-required" {
		t.Fatalf("task states = %v, want the turn to pause", got)
	}
	call := confirmationCall(t, last)
	resumed := streamMethod(t, server, a2aserver.RootPath, "message/stream",
		confirmationMessage(last, call, "verified operator approved the simulated restart"), authenticated)
	if got := states(resumed); got[len(got)-1] != "completed" {
		t.Fatalf("resumed states = %v, want a completed action", got)
	}

	name, err := domain.NormalizeSlug(inventory)
	if err != nil {
		t.Fatalf("NormalizeSlug() error = %v, want nil", err)
	}
	service, err := fixture.store.GetService(t.Context(), name)
	if err != nil || service == nil {
		t.Fatalf("GetService() = %+v, %v, want %s", service, err, inventory)
	}
	if service.Status() != domain.ServiceStatusOperational {
		t.Errorf("%s status = %q, want %q", inventory, service.Status(), domain.ServiceStatusOperational)
	}
	pool := openRaw(t, filepath.Join(fixture.stateDir, "incidents.db"))
	var approvedBy string
	if err := pool.QueryRowContext(t.Context(),
		"SELECT approved_by FROM audit_log ORDER BY id DESC LIMIT 1").Scan(&approvedBy); err != nil {
		t.Fatalf("read audit approver: %v", err)
	}
	if approvedBy != "alice@example.test" {
		t.Errorf("audit approved_by = %q, want authenticated subject", approvedBy)
	}
}

func newRealRestartFixture(t *testing.T, model *scriptedLLM, trustedHeader string) *fixture {
	t.Helper()

	return newFixture(t, func(opts *fixtureOptions) {
		opts.model = model
		opts.options = func(options *a2aserver.Options) { options.TrustedIdentityHeader = trustedHeader }
		opts.agentFactory = func(script *scriptedLLM, store *data.Store) agent.Agent {
			realTools, err := agenttools.New(agenttools.Config{
				Store: store,
				Guard: func(ctx context.Context, _ string, call func(context.Context) error) error {
					return call(ctx)
				},
				Redact: func(text string) string { return text },
			})
			if err != nil {
				t.Fatalf("tools.New() error = %v, want nil", err)
			}
			return newAgent(t, script, func(cfg *llmagent.Config) {
				cfg.Tools = []tool.Tool{realTools.RestartService()}
			})
		}
	})
}

func confirmationMessage(last, call map[string]any, rationale string) map[string]any {
	return map[string]any{"message": map[string]any{
		"kind":      "message",
		"messageId": "m2",
		"role":      "user",
		"contextId": last["contextId"],
		"taskId":    last["taskId"],
		"parts": []any{map[string]any{
			"kind": "data",
			"data": map[string]any{
				"id":   call["id"],
				"name": call["name"],
				"response": map[string]any{
					"confirmed": true,
					"payload":   map[string]any{"rationale": rationale},
				},
			},
			"metadata": map[string]any{"adk_type": "function_response"},
		}},
	}}
}

// TestSessionSurvivesAcrossTurns proves the contextId is the session: the
// second turn's request carries the first turn's exchange, which is what makes
// a conversation over A2A a conversation.
func TestSessionSurvivesAcrossTurns(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(opts *fixtureOptions) {
		opts.model = &scriptedLLM{turns: [][]*adkmodel.LLMResponse{
			textTurn("First answer."), textTurn("Second answer."),
		}}
	})
	server := fixture.serve(t)

	first := streamResults(t, server, textMessage("m1", "First question."))
	contextID := first[0]["contextId"]

	second := streamResults(t, server, map[string]any{"message": map[string]any{
		"kind":      "message",
		"messageId": "m2",
		"role":      "user",
		"contextId": contextID,
		"parts":     []any{map[string]any{"kind": "text", "text": "Second question."}},
	}})
	if second[0]["contextId"] != contextID {
		t.Errorf("second turn context = %v, want %v", second[0]["contextId"], contextID)
	}
	// A second task under one context: the conversation continues, the unit of
	// work does not.
	if second[0]["id"] == first[0]["id"] {
		t.Error("the second turn reused the first turn's task id")
	}

	requests := fixture.model.observedRequests()
	if len(requests) != 2 {
		t.Fatalf("the model was called %d times, want 2", len(requests))
	}
	if !requestMentions(requests[1], "First question.") || !requestMentions(requests[1], "First answer.") {
		t.Error("the second request did not carry the first turn's exchange")
	}
}

// TestTaskIsPersistedAndSurvivesARestart is the G-4 requirement at the wire
// level: the task a client created is still readable by a process that starts
// afterwards.
func TestTaskIsPersistedAndSurvivesARestart(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(opts *fixtureOptions) {
		opts.options = func(options *a2aserver.Options) { options.TrustedIdentityHeader = identityHeader }
	})
	server := fixture.serve(t)
	authenticated := [2]string{identityHeader, testOwner}
	results := streamMethod(t, server, a2aserver.RootPath, "message/stream",
		textMessage("m1", "Hello."), authenticated)
	taskID, _ := results[0]["id"].(string)
	if taskID == "" {
		t.Fatal("the stream produced no task id")
	}

	// Close the server the way a rolling update would, then open a new task
	// store on the same file — a second process, the same state.
	if err := fixture.server.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	reopened := openTaskStoreAt(t, filepath.Join(fixture.stateDir, a2aserver.TaskDatabaseName))
	stored := mustGet(t, reopened, a2a.TaskID(taskID))
	if stored.Task.Status.State.String() != "TASK_STATE_COMPLETED" {
		t.Errorf("persisted state = %q, want the completed task", stored.Task.Status.State)
	}
	if stored.Version < 1 {
		t.Errorf("persisted version = %d, want a tracked version", stored.Version)
	}
}

// TestCancellationEndsTheTaskCanceled covers the behavior Python needed a
// third executor subclass for. ADK Go's executor already publishes the terminal
// canceled event, so there is nothing to subclass — but the guarantee still has
// to be proven end to end.
func TestCancellationEndsTheTaskCanceled(t *testing.T) {
	// This five-second assertion measures cancellation, not scheduler or SQLite
	// throughput. Keep the integration fixture serial inside this package so the
	// race suite cannot spend its entire deadline opening parallel databases.

	started := make(chan struct{})
	model := &scriptedLLM{
		turns: [][]*adkmodel.LLMResponse{textTurn("too late")},
		hold: map[int]func(ctx context.Context){0: func(ctx context.Context) {
			close(started)
			<-ctx.Done()
		}},
	}
	fixture := newFixture(t, func(opts *fixtureOptions) {
		opts.model = model
		opts.options = func(options *a2aserver.Options) { options.TrustedIdentityHeader = identityHeader }
	})
	server := fixture.serve(t)
	authenticated := [2]string{identityHeader, testOwner}

	streamed := make(chan []map[string]any, 1)
	go func() {
		streamed <- streamMethod(t, server, a2aserver.RootPath, "message/stream",
			textMessage("m1", "Wait for cancellation."), authenticated)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the model was never called")
	}

	taskID := latestTaskID(t, fixture)
	envelope := rpc(t, server, a2aserver.RootPath, "tasks/cancel", map[string]any{"id": taskID}, authenticated)
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		t.Fatalf("tasks/cancel returned %v, want a task", envelope)
	}
	status, _ := result["status"].(map[string]any)
	if got := status["state"]; got != "canceled" {
		t.Errorf("cancel state = %v, want %q", got, "canceled")
	}

	select {
	case results := <-streamed:
		if got := states(results); len(got) == 0 || got[len(got)-1] != "canceled" {
			t.Errorf("stream states = %v, want the stream to end canceled", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never terminated after the cancellation")
	}

	// The terminal state is durable, not only streamed: an operator inspecting
	// the store through the same authenticated owner must see the same answer.
	inspector := openTaskStoreAt(t, filepath.Join(fixture.stateDir, a2aserver.TaskDatabaseName))
	stored := mustGet(t, inspector, a2a.TaskID(taskID))
	if stored.Task.Status.State.String() != "TASK_STATE_CANCELED" {
		t.Errorf("persisted state = %q, want the canceled task", stored.Task.Status.State)
	}
}

// TestGovernedModelFailureEndsTerminallyWithoutLeakingTheCause is the Python
// interceptor's guarantee, met by ADK Go without one: the terminal event
// carries the structured error code, the caller sees the policy plane's safe
// wording, and the provider's own message never reaches the wire.
func TestGovernedModelFailureEndsTerminallyWithoutLeakingTheCause(t *testing.T) {
	t.Parallel()

	const secret = "connection refused to 10.1.2.3:11434 while reading /var/lib/secrets"
	model := &scriptedLLM{
		turns: [][]*adkmodel.LLMResponse{nil},
		fail:  map[int]error{0: errors.New(secret)},
	}
	fixture := newFixture(t, func(opts *fixtureOptions) {
		opts.model = model
		opts.plugins = []*plugin.Plugin{governancePlugin(t)}
	})
	server := fixture.serve(t)

	results := streamResults(t, server, textMessage("m1", "Hello."))
	final := results[len(results)-1]
	if got := states(results); got[len(got)-1] != "failed" {
		t.Errorf("task states = %v, want the turn to fail", got)
	}
	if final["final"] != true {
		t.Errorf("final = %v on the terminal event, want true", final["final"])
	}
	metadata, _ := final["metadata"].(map[string]any)
	if got := metadata["adk_error_code"]; got != "MODEL_UNAVAILABLE" {
		t.Errorf("adk_error_code = %v, want %q", got, "MODEL_UNAVAILABLE")
	}
	encoded, err := json.Marshal(results)
	if err != nil {
		t.Fatalf("re-encoding the stream: %v", err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Error("the provider's raw failure reached the wire")
	}
	if !strings.Contains(string(encoded), "Model request failed safely.") {
		t.Error("the stream does not carry the policy plane's safe wording")
	}
}

// TestModelCallBudgetBoundsARunawayTurn is AGENT_A2A_MAX_LLM_CALLS. ADK Go has
// no framework bound outside the live path, so this proves the plugin that
// replaces it actually stops a model that keeps planning.
func TestModelCallBudgetBoundsARunawayTurn(t *testing.T) {
	t.Parallel()

	const budget = 3
	loopTool := newLoopingTool(t)
	turns := make([][]*adkmodel.LLMResponse, 0, 20)
	for index := range 20 {
		turns = append(turns, toolCallTurn("loop-"+string(rune('a'+index)), loopingToolName, map[string]any{}))
	}
	model := &scriptedLLM{turns: turns}
	fixture := newFixture(t, func(opts *fixtureOptions) {
		opts.model = model
		opts.agent = newAgent(t, model, func(cfg *llmagent.Config) { cfg.Tools = []tool.Tool{loopTool} })
		opts.options = func(options *a2aserver.Options) { options.MaxLLMCalls = budget }
	})
	server := fixture.serve(t)

	results := streamResults(t, server, textMessage("m1", "Loop forever."))
	if got := states(results); got[len(got)-1] != "failed" {
		t.Errorf("task states = %v, want the runaway to fail", got)
	}
	if got := fixture.model.callCount(); got != budget {
		t.Errorf("the model was called %d times, want the budget of %d", got, budget)
	}
}

// TestAnUnboundedBudgetInstallsNoPlugin keeps the opt-out honest: zero means
// unbounded, and an always-true guard on the hottest callback is worth not
// installing.
func TestAnUnboundedBudgetInstallsNoPlugin(t *testing.T) {
	t.Parallel()

	loopTool := newLoopingTool(t)
	model := &scriptedLLM{turns: [][]*adkmodel.LLMResponse{
		toolCallTurn("loop-1", loopingToolName, map[string]any{}),
		textTurn("Finished."),
	}}
	fixture := newFixture(t, func(opts *fixtureOptions) {
		opts.model = model
		opts.agent = newAgent(t, model, func(cfg *llmagent.Config) { cfg.Tools = []tool.Tool{loopTool} })
		opts.options = func(options *a2aserver.Options) { options.MaxLLMCalls = 0 }
	})
	server := fixture.serve(t)

	results := streamResults(t, server, textMessage("m1", "Use the tool once."))
	if got := states(results); got[len(got)-1] != "completed" {
		t.Errorf("task states = %v, want an unbounded turn to complete", got)
	}
	if got := fixture.model.callCount(); got != 2 {
		t.Errorf("the model was called %d times, want 2", got)
	}
}

// TestStreamingOptInEmitsIncrementalArtifacts is Chapter 3.6's trade-off: with
// AGENT_A2A_STREAMING off a client still gets whole task events, and with it on
// the model's own tokens arrive as they are produced.
func TestStreamingOptInEmitsIncrementalArtifacts(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name string
		// One artifact update per model event. Streaming adds the partial
		// chunk and the empty LastChunk update adka2a appends to reset it.
		wantArtifacts int
		streaming     bool
		wantPartial   bool
	}{
		{"off", 1, false, false},
		{"on", 3, true, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			model := &scriptedLLM{turns: [][]*adkmodel.LLMResponse{{
				{
					Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "first "}}},
					Partial: true,
				},
				{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: "first second"}}}},
			}}}
			fixture := newFixture(t, func(opts *fixtureOptions) {
				opts.model = model
				opts.options = func(options *a2aserver.Options) { options.Streaming = testCase.streaming }
			})
			server := fixture.serve(t)

			results := streamResults(t, server, textMessage("m1", "Stream a reply."))
			var artifacts []map[string]any
			for _, result := range results {
				if result["kind"] == "artifact-update" {
					artifacts = append(artifacts, result)
				}
			}
			if len(artifacts) != testCase.wantArtifacts {
				t.Errorf("artifact updates = %d, want %d", len(artifacts), testCase.wantArtifacts)
			}
			if got := states(results); got[len(got)-1] != "completed" {
				t.Errorf("task states = %v, want the turn to complete", got)
			}
			// With streaming on the caller sees the model's first chunk before
			// the turn is finished, which is the whole trade-off: earlier text,
			// and a redactor that has already emitted the prefix.
			wantText := "first second"
			if testCase.wantPartial {
				wantText = "first "
			}
			if got := artifactText(artifacts[0]); got != wantText {
				t.Errorf("first artifact text = %q, want %q", got, wantText)
			}
			metadata, _ := artifacts[0]["metadata"].(map[string]any)
			if got := metadata["adk_partial"]; got != testCase.wantPartial {
				t.Errorf("adk_partial = %v, want %v", got, testCase.wantPartial)
			}
		})
	}
}

// TestStreamedChunksCrossTheRedactionBoundary is why AGENT_A2A_STREAMING is
// still off on the taught path, stated as a measurement rather than a warning.
//
// The redactor runs on every model response, partials included, but it can only
// ever see one response at a time. ADK's OpenAI-compatible adapter — the path
// local Ollama takes — yields one delta per chunk and one aggregated response at
// the end, which is what the script below reproduces. An address split across
// two deltas is therefore invisible in both fragments and caught only in the
// aggregate, by which time the fragments are already on the wire.
func TestStreamedChunksCrossTheRedactionBoundary(t *testing.T) {
	t.Parallel()

	// Neither fragment is an address: "10." has one octet and "1.2.3" has three.
	// Joined, they are the address the boundary policy masks.
	const (
		address     = "10.1.2.3"
		firstDelta  = "The failing host is 10."
		secondDelta = "1.2.3, so page the on-call."
	)
	whole := firstDelta + secondDelta

	for _, testCase := range []struct {
		name string
		// wantLeak is whether the chunks a client has already rendered
		// reassemble into the address the final message masks.
		streaming bool
		wantLeak  bool
	}{
		{name: "off", streaming: false, wantLeak: false},
		{name: "on", streaming: true, wantLeak: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			model := &scriptedLLM{turns: [][]*adkmodel.LLMResponse{{
				{Partial: true, Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: firstDelta}}}},
				{Partial: true, Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: secondDelta}}}},
				{Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: whole}}}},
			}}}
			fixture := newFixture(t, func(opts *fixtureOptions) {
				opts.model = model
				// The shipped policy plane, not a stand-in: the claim being
				// measured is about the redaction learners actually run.
				opts.plugins = []*plugin.Plugin{governancePlugin(t)}
				opts.options = func(options *a2aserver.Options) { options.Streaming = testCase.streaming }
			})
			server := fixture.serve(t)

			results := streamResults(t, server, textMessage("m1", "Which host is failing?"))
			var streamed strings.Builder
			var final string
			for _, result := range results {
				if result["kind"] != "artifact-update" {
					continue
				}
				if artifactIsPartial(result) {
					streamed.WriteString(artifactText(result))
					continue
				}
				final = artifactText(result)
			}

			// The whole message is masked either way. That is the guarantee the
			// course teaches, and streaming does not take it away.
			if strings.Contains(final, address) {
				t.Errorf("final artifact = %q, want the address masked", final)
			}
			if !strings.Contains(final, "<IP_ADDRESS>") {
				t.Errorf("final artifact = %q, want it to carry the mask", final)
			}

			// What streaming takes away is the whole message being the only thing
			// the client ever saw.
			if got := strings.Contains(streamed.String(), address); got != testCase.wantLeak {
				t.Errorf("streamed chunks %q contain %q = %v, want %v",
					streamed.String(), address, got, testCase.wantLeak)
			}

			// Logged rather than only asserted: `go test -v` on this case is the
			// shortest way to see both texts side by side.
			t.Logf("chunks the client rendered: %q", streamed.String())
			t.Logf("whole message after redaction: %q", final)
		})
	}
}

// TestACancelableTaskIDArrivesBeforeTheModelAnswers is the difference between a
// spinner and a stream, measured at the wire.
//
// A client that waits for message/send has no task id until the turn is over,
// so its Cancel button can never do anything. The same turn over message/stream
// hands the id to the client in the first frame — with AGENT_A2A_STREAMING off,
// because task events and model tokens are separate switches.
func TestACancelableTaskIDArrivesBeforeTheModelAnswers(t *testing.T) {
	// Serial like the other held-model test in this package: it measures
	// cancellation, not how fast a parallel suite can open SQLite databases.

	started := make(chan struct{})
	model := &scriptedLLM{
		turns: [][]*adkmodel.LLMResponse{textTurn("The answer nobody waited for.")},
		hold: map[int]func(ctx context.Context){0: func(ctx context.Context) {
			close(started)
			<-ctx.Done()
		}},
	}
	fixture := newFixture(t, func(opts *fixtureOptions) {
		opts.model = model
		opts.options = func(options *a2aserver.Options) { options.Streaming = false }
	})
	server := fixture.serve(t)

	request := streamRequest{
		path:   a2aserver.RootPath,
		method: "message/stream",
		params: textMessage("m1", "Take your time."),
	}
	withStream(t, server, request, func(stream *liveStream) {
		first := stream.next()
		taskID, ok := first["id"].(string)
		if !ok || taskID == "" {
			t.Fatalf("first frame = %v, want a task carrying an id", first)
		}
		if got := first["kind"]; got != "task" {
			t.Errorf("first frame kind = %v, want %q", got, "task")
		}

		// The id is in the client's hands while the model is still thinking,
		// which is the whole point: there is something to cancel.
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the model was never called")
		}

		envelope := rpc(t, server, a2aserver.RootPath, "tasks/cancel", map[string]any{"id": taskID})
		if _, ok := envelope["result"].(map[string]any); !ok {
			t.Fatalf("tasks/cancel returned %v, want a task", envelope)
		}

		var remaining []map[string]any
		for event := stream.next(); event != nil; event = stream.next() {
			remaining = append(remaining, event)
		}
		if got := states(remaining); len(got) == 0 || got[len(got)-1] != "canceled" {
			t.Errorf("remaining stream states = %v, want the stream to end canceled", got)
		}
		for _, event := range remaining {
			if strings.Contains(artifactText(event), "The answer") {
				t.Errorf("the canceled turn still delivered its answer: %v", event)
			}
		}
	})
}

// TestClosingTheConnectionDoesNotCancelTheTask separates leaving from stopping.
//
// a2a-go runs an execution on context.WithoutCancel of the request context
// (internal/taskexec/local_manager.go), so a browser tab that closes mid-turn
// abandons the stream and nothing else. The turn keeps burning model calls
// until tasks/cancel says otherwise, which is why the client has a Cancel
// button rather than a reload instruction.
func TestClosingTheConnectionDoesNotCancelTheTask(t *testing.T) {
	// Serial: it holds the model until the test lets go.

	started := make(chan struct{})
	released := make(chan struct{})
	model := &scriptedLLM{
		turns: [][]*adkmodel.LLMResponse{textTurn("Finished after the client walked away.")},
		hold: map[int]func(ctx context.Context){0: func(ctx context.Context) {
			close(started)
			select {
			case <-released:
			case <-ctx.Done():
			}
		}},
	}
	fixture := newFixture(t, func(opts *fixtureOptions) {
		opts.model = model
		// The store answers "not found" for a task another subject owns, so the
		// turn is submitted under the same identity the inspector reads with.
		opts.options = func(options *a2aserver.Options) { options.TrustedIdentityHeader = identityHeader }
	})
	server := fixture.serve(t)

	request := streamRequest{
		path:    a2aserver.RootPath,
		method:  "message/stream",
		params:  textMessage("m1", "Take your time."),
		headers: [][2]string{{identityHeader, testOwner}},
	}
	var taskID string
	withStream(t, server, request, func(stream *liveStream) {
		first := stream.next()
		id, ok := first["id"].(string)
		if !ok || id == "" {
			t.Fatalf("first frame = %v, want a task carrying an id", first)
		}
		taskID = id
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the model was never called")
		}
		stream.close() // the tab closes
	})
	close(released)

	inspector := openTaskStoreAt(t, filepath.Join(fixture.stateDir, a2aserver.TaskDatabaseName))
	deadline := time.Now().Add(5 * time.Second)
	var final string
	for time.Now().Before(deadline) {
		final = mustGet(t, inspector, a2a.TaskID(taskID)).Task.Status.State.String()
		if final == "TASK_STATE_COMPLETED" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("state after the client disconnected = %q, want the turn to have finished anyway", final)
}

// TestAgentCardIsPublicAndHidesTheInstruction is the discovery contract. ADK's
// own BuildAgentSkills would publish the agent's operating instruction,
// rewritten into the first person; this card is assembled by hand precisely so
// that cannot happen.
func TestAgentCardIsPublicAndHidesTheInstruction(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(opts *fixtureOptions) {
		opts.options = func(options *a2aserver.Options) {
			options.Host = "agent.example"
			options.Port = 9443
			options.Protocol = "https"
			options.Version = "9.9.9"
		}
	})
	server := fixture.serve(t)

	response, err := server.Client().Get(server.URL + a2aserver.CardPath)
	if err != nil {
		t.Fatalf("GET %s: %v", a2aserver.CardPath, err)
	}
	defer func() { _ = response.Body.Close() }()
	if got := response.StatusCode; got != http.StatusOK {
		t.Fatalf("status = %d, want %d", got, http.StatusOK)
	}
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the card: %v", err)
	}
	var card map[string]any
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatalf("decoding the card: %v", err)
	}

	if card["name"] != a2aserver.CardName || card["version"] != "9.9.9" {
		t.Errorf("card identity = %v/%v, want %q/%q", card["name"], card["version"], a2aserver.CardName, "9.9.9")
	}
	if card["description"] != a2aserver.CardDescription {
		t.Errorf("description = %v, want the curated one", card["description"])
	}
	// The instruction the test agent carries must be nowhere in the document.
	if strings.Contains(string(raw), "Operating rules") {
		t.Error("the agent instruction leaked into the public card")
	}
	capabilities, _ := card["capabilities"].(map[string]any)
	if capabilities["streaming"] != true {
		t.Errorf("capabilities = %v, want streaming advertised", capabilities)
	}
	skills, _ := card["skills"].([]any)
	skillIDs := make([]string, 0, len(skills))
	for _, entry := range skills {
		skill, _ := entry.(map[string]any)
		id, _ := skill["id"].(string)
		skillIDs = append(skillIDs, id)
	}
	want := []string{a2aserver.TriageSkillID, a2aserver.RemediationSkillID}
	slices.Sort(skillIDs)
	slices.Sort(want)
	if !slices.Equal(skillIDs, want) {
		t.Errorf("skills = %v, want %v", skillIDs, want)
	}
	// The 0.3 union fields: a compat client reads the top-level url, a 1.0
	// client reads supportedInterfaces. Both describe endpoints this server
	// actually serves, on the advertised host rather than the bind address.
	if got := card["url"]; got != "https://agent.example:9443"+a2aserver.RootPath {
		t.Errorf("preferred url = %v, want the advertised root", got)
	}
	interfaces, _ := card["supportedInterfaces"].([]any)
	urls := make([]string, 0, len(interfaces))
	for _, entry := range interfaces {
		iface, _ := entry.(map[string]any)
		url, _ := iface["url"].(string)
		urls = append(urls, url)
	}
	wantURLs := []string{
		"https://agent.example:9443" + a2aserver.RootPath,
		"https://agent.example:9443" + a2aserver.InvokePath,
	}
	if !slices.Equal(urls, wantURLs) {
		t.Errorf("supportedInterfaces = %v, want %v", urls, wantURLs)
	}
}

// TestTheCardSendsNoCORSHeaders records a deliberate divergence from a2a-go's
// own card handler, which reflects any Origin with credentials.
//
// The Python server sends no CORS headers at all, and the browser client is
// documented as needing the gateway in front of it — which is where the origin
// allowlist lives. Reflecting any origin here would quietly make that policy
// optional.
func TestTheCardSendsNoCORSHeaders(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	server := fixture.serve(t)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+a2aserver.CardPath, nil)
	if err != nil {
		t.Fatalf("building the card request: %v", err)
	}
	request.Header.Set("Origin", "https://attacker.example")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", a2aserver.CardPath, err)
	}
	defer func() { _ = response.Body.Close() }()
	for _, header := range []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Credentials",
	} {
		if got := response.Header.Get(header); got != "" {
			t.Errorf("%s = %q, want no CORS header", header, got)
		}
	}
}

// TestBothProtocolBindingsServeTheSameHandler proves the 1.0 and 0.3 paths are
// two wire shapes over one implementation rather than two implementations.
func TestBothProtocolBindingsServeTheSameHandler(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, func(opts *fixtureOptions) {
		opts.model = &scriptedLLM{turns: [][]*adkmodel.LLMResponse{
			textTurn("One."), textTurn("Two."), textTurn("Three."), textTurn("Four."), textTurn("Five."),
		}}
	})
	server := fixture.serve(t)

	for _, binding := range []struct{ path, method string }{
		{a2aserver.RootPath, "message/send"},
		{a2aserver.CompatInvokePath, "message/send"},
		{a2aserver.InvokePath, "SendMessage"},
	} {
		t.Run(binding.path+" "+binding.method, func(t *testing.T) {
			envelope := rpc(t, server, binding.path, binding.method, textMessage("m-"+binding.method, "Hello."))
			if envelope["error"] != nil {
				t.Fatalf("%s returned %v, want a result", binding.method, envelope["error"])
			}
			if envelope["result"] == nil {
				t.Errorf("%s returned no result", binding.method)
			}
		})
	}

	// Streaming is named differently on each binding and shaped differently on
	// the wire: 0.3 flattens each event, 1.0 tags it. Both must describe the
	// same completed turn.
	t.Run(a2aserver.CompatInvokePath+" message/stream", func(t *testing.T) {
		results := streamMethod(t, server, a2aserver.CompatInvokePath, "message/stream",
			textMessage("s-compat", "Hello."))
		if got := states(results); len(got) == 0 || got[len(got)-1] != "completed" {
			t.Errorf("message/stream states = %v, want the turn to complete", got)
		}
	})
	t.Run(a2aserver.InvokePath+" SendStreamingMessage", func(t *testing.T) {
		results := streamMethod(t, server, a2aserver.InvokePath, "SendStreamingMessage",
			textMessage("s-modern", "Hello."))
		if len(results) == 0 {
			t.Fatal("SendStreamingMessage produced no events")
		}
		// The 1.0 result is a tagged union and the states carry their protocol
		// enum names, where 0.3 flattens both.
		update, ok := results[len(results)-1]["statusUpdate"].(map[string]any)
		if !ok {
			t.Fatalf("the terminal 1.0 event = %v, want a statusUpdate", results[len(results)-1])
		}
		status, _ := update["status"].(map[string]any)
		if got := status["state"]; got != "TASK_STATE_COMPLETED" {
			t.Errorf("terminal state = %v, want %q", got, "TASK_STATE_COMPLETED")
		}
	})
}

// TestUnroutedPathsAreNotTheProtocol guards the root mount: "POST /" anchored
// with {$} must not become a catch-all that answers every unrouted path with
// the agent.
func TestUnroutedPathsAreNotTheProtocol(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	server := fixture.serve(t)

	response := post(t, server, "/not-a-route", "message/send", textMessage("m1", "Hello."))
	defer func() { _ = response.Body.Close() }()
	if got := response.StatusCode; got != http.StatusNotFound {
		t.Errorf("status = %d, want %d", got, http.StatusNotFound)
	}
	if fixture.model.callCount() != 0 {
		t.Errorf("the model ran %d times for an unrouted path, want 0", fixture.model.callCount())
	}
}

// governancePlugin builds the real policy plane, so a wire assertion about
// error hygiene is an assertion about the shipped rules.
func governancePlugin(t *testing.T) *plugin.Plugin {
	t.Helper()

	governance, err := policy.New(policy.Config{})
	if err != nil {
		t.Fatalf("policy.New() error = %v, want nil", err)
	}
	built, err := governance.Plugin()
	if err != nil {
		t.Fatalf("policy.Plugin() error = %v, want nil", err)
	}
	return built
}

// The two stand-in tools the protocol tests bind. Their names are local to this
// package: the real guarded actions live in the tools package and are proven
// there, and what these exercise is the protocol around a tool, not the tool.
const (
	confirmedToolName = "confirmed_action"
	loopingToolName   = "looping_action"
)

type confirmedArgs struct {
	Name string `json:"name" jsonschema:"the target of the guarded action"`
}

type confirmedResult struct {
	Status string `json:"status"`
}

// newConfirmingTool builds a tool that pauses for human approval.
func newConfirmingTool(t *testing.T, record func(name string)) tool.Tool {
	t.Helper()

	built, err := functiontool.New(
		functiontool.Config{
			Name:                confirmedToolName,
			Description:         "Perform a guarded action after human approval.",
			RequireConfirmation: true,
		},
		func(_ agent.Context, args confirmedArgs) (confirmedResult, error) {
			record(args.Name)
			return confirmedResult{Status: "done"}, nil
		},
	)
	if err != nil {
		t.Fatalf("functiontool.New(%q) error = %v, want nil", confirmedToolName, err)
	}
	return built
}

type loopingResult struct {
	Status string `json:"status"`
}

// newLoopingTool builds a tool that always succeeds, so a scripted model can be
// made to plan forever.
func newLoopingTool(t *testing.T) tool.Tool {
	t.Helper()

	built, err := functiontool.New(
		functiontool.Config{Name: loopingToolName, Description: "Always succeeds."},
		func(_ agent.Context, _ struct{}) (loopingResult, error) {
			return loopingResult{Status: "ok"}, nil
		},
	)
	if err != nil {
		t.Fatalf("functiontool.New(%q) error = %v, want nil", loopingToolName, err)
	}
	return built
}

// toolCallTurn scripts one model turn that calls a tool.
func toolCallTurn(callID, name string, args map[string]any) []*adkmodel.LLMResponse {
	return []*adkmodel.LLMResponse{{
		Content: &genai.Content{Role: "model", Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{ID: callID, Name: name, Args: args},
		}}},
	}}
}

// artifactText returns the text of an artifact update's first part.
func artifactText(event map[string]any) string {
	artifact, _ := event["artifact"].(map[string]any)
	parts, _ := artifact["parts"].([]any)
	if len(parts) == 0 {
		return ""
	}
	part, _ := parts[0].(map[string]any)
	text, _ := part["text"].(string)
	return text
}

// artifactIsPartial reports whether an artifact update carries a model chunk
// rather than the finished message. ADK marks the difference in the event's own
// metadata so a client does not have to guess.
func artifactIsPartial(event map[string]any) bool {
	metadata, _ := event["metadata"].(map[string]any)
	partial, _ := metadata["adk_partial"].(bool)
	return partial
}

// confirmationCall extracts the confirmation request from a paused task.
func confirmationCall(t *testing.T, event map[string]any) map[string]any {
	t.Helper()

	status, _ := event["status"].(map[string]any)
	message, _ := status["message"].(map[string]any)
	parts, _ := message["parts"].([]any)
	for _, entry := range parts {
		part, _ := entry.(map[string]any)
		metadata, _ := part["metadata"].(map[string]any)
		if metadata["adk_type"] != "function_call" {
			continue
		}
		data, _ := part["data"].(map[string]any)
		if data["name"] == "adk_request_confirmation" {
			return data
		}
	}
	t.Fatalf("the paused task carries no confirmation request: %v", event)
	return nil
}

// TestTheCardRejectsANonGetRequest keeps the discovery path read-only: a card is
// something to fetch, and answering a POST there would be a second, unasked-for
// way into the server.
func TestTheCardRejectsANonGetRequest(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t)
	server := fixture.serve(t)

	response := post(t, server, a2aserver.CardPath, "message/send", textMessage("m1", "Hello."))
	defer func() { _ = response.Body.Close() }()
	if got := response.StatusCode; got != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", got, http.StatusMethodNotAllowed)
	}
}
