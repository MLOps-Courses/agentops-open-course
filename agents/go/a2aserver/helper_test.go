package a2aserver_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/glebarez/go-sqlite"
	"github.com/glebarez/sqlite"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"google.golang.org/genai"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/a2aserver"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/data"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/state"
)

// Every test in this package is offline and deterministic: no model is called
// over a network, no port is bound except by the in-process httptest servers a
// test starts itself, and the dataset is a per-test copy of the committed one.

// repositoryDataset is the committed dataset, relative to this package.
const repositoryDataset = "../../data"

// scriptedLLM is a model.LLM that replays a fixed script.
//
// Each turn is one entry. A turn that yields several responses is a stream; a
// turn that yields an error interrupts mid-stream, which is how the terminal
// failure path is exercised without a real outage.
type scriptedLLM struct {
	fail map[int]error
	// hold blocks a turn until the test lets it go, which is how a
	// cancellation can be sent while a turn is genuinely in flight.
	hold  map[int]func(ctx context.Context)
	turns [][]*adkmodel.LLMResponse
	// requests records the request of each call so a test can assert the tool
	// results were fed back.
	requests []*adkmodel.LLMRequest

	guard sync.Mutex
	// calls counts GenerateContent invocations, which is what proves the call
	// budget bounded a turn.
	calls int
}

func (m *scriptedLLM) Name() string { return "scripted-test-model" }

func (m *scriptedLLM) GenerateContent(
	ctx context.Context, req *adkmodel.LLMRequest, stream bool,
) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.guard.Lock()
	index := m.calls
	m.calls++
	m.requests = append(m.requests, req)
	m.guard.Unlock()

	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if block, ok := m.hold[index]; ok {
			block(ctx)
		}
		if index < len(m.turns) {
			for _, response := range m.turns[index] {
				// A real adapter emits partial chunks only when the caller asked
				// to stream. Honoring the flag is what makes the streaming
				// opt-in assertion mean something.
				if response.Partial && !stream {
					continue
				}
				if !yield(response, nil) {
					return
				}
			}
		}
		if err, ok := m.fail[index]; ok {
			yield(nil, err)
			return
		}
		if index >= len(m.turns) {
			yield(nil, fmt.Errorf("scriptedLLM: no scripted turn %d", index))
		}
	}
}

// callCount reports how many times the model was asked for a completion.
func (m *scriptedLLM) callCount() int {
	m.guard.Lock()
	defer m.guard.Unlock()
	return m.calls
}

// observedRequests returns the request of every model call, so a test can prove
// what the agent actually sent.
func (m *scriptedLLM) observedRequests() []*adkmodel.LLMRequest {
	m.guard.Lock()
	defer m.guard.Unlock()
	return append([]*adkmodel.LLMRequest(nil), m.requests...)
}

// requestMentions reports whether a model request carries the given text
// anywhere in its conversation history.
func requestMentions(request *adkmodel.LLMRequest, text string) bool {
	for _, content := range request.Contents {
		for _, part := range content.Parts {
			if strings.Contains(part.Text, text) {
				return true
			}
		}
	}
	return false
}

// latestTaskID reads the most recently written task id straight out of the task
// database.
//
// Reading the file rather than the API is deliberate: at this point the turn is
// still in flight, the listing path needs an authenticated caller, and the
// question being asked — "did the server persist this task before it finished?"
// — is about the file.
func latestTaskID(t *testing.T, fixture *fixture) string {
	t.Helper()

	pool := openRaw(t, filepath.Join(fixture.stateDir, a2aserver.TaskDatabaseName))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var id string
		err := pool.QueryRowContext(t.Context(),
			`SELECT id FROM tasks ORDER BY rowid DESC LIMIT 1`).Scan(&id)
		if err == nil {
			return id
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("reading the latest task id: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no task was persisted before the deadline")
	return ""
}

// textTurn scripts one non-streaming answer.
func textTurn(text string) []*adkmodel.LLMResponse {
	return []*adkmodel.LLMResponse{{
		Content: &genai.Content{Role: "model", Parts: []*genai.Part{{Text: text}}},
	}}
}

// recordedSteps captures the startup sequence so its order can be asserted.
type recordedSteps struct {
	// failures replaces one step's outcome, so a startup rejection can be
	// exercised without corrupting a real database.
	failures map[string]error
	order    []string
	guard    sync.Mutex
}

func newRecordedSteps() *recordedSteps {
	return &recordedSteps{failures: map[string]error{}}
}

// step wraps one real startup action, recording that it ran.
func (r *recordedSteps) step(name string, action a2aserver.Step) a2aserver.Step {
	return func(ctx context.Context) error {
		r.guard.Lock()
		r.order = append(r.order, name)
		failure, failed := r.failures[name]
		r.guard.Unlock()
		if failed {
			return failure
		}
		return action(ctx)
	}
}

// fail makes one startup step report a failure instead of running.
func (r *recordedSteps) fail(name string, err error) {
	r.guard.Lock()
	defer r.guard.Unlock()
	r.failures[name] = err
}

func (r *recordedSteps) sequence() []string {
	r.guard.Lock()
	defer r.guard.Unlock()
	return append([]string(nil), r.order...)
}

// fixture is one isolated dataset, state directory, agent and server.
type fixture struct {
	server   *a2aserver.Server
	model    *scriptedLLM
	store    *data.Store
	steps    *recordedSteps
	stateDir string
}

// fixtureOptions tunes a fixture.
type fixtureOptions struct {
	// options overrides the server settings before they are resolved.
	options func(*a2aserver.Options)
	// agent replaces the served agent.
	agent agent.Agent
	// agentFactory builds an agent over the same real store the server prepares.
	agentFactory func(*scriptedLLM, *data.Store) agent.Agent
	// model scripts the fake model.
	model *scriptedLLM
	// sessions replaces the session service.
	sessions session.Service
	// promptGuard is mounted on the shared private listener outside A2A
	// identity binding.
	promptGuard http.Handler
	// promptGuardReadiness replaces the default successful configured-model
	// readiness check.
	promptGuardReadiness a2aserver.Step
	// probeSessions supplies the session-store readiness check a non-file
	// backend requires. The file backend refuses one, so it stays nil there.
	probeSessions a2aserver.Step
	// plugins are installed alongside the call budget. The slice stays last to
	// keep the garbage collector's pointer scan compact.
	plugins []*plugin.Plugin
}

// newFixture builds a server over a fresh dataset copy and starts it.
func newFixture(t *testing.T, configure ...func(*fixtureOptions)) *fixture {
	t.Helper()

	fixture := newUnstartedFixture(t, configure...)
	if !fixture.started() {
		if err := fixture.server.Start(t.Context()); err != nil {
			t.Fatalf("Start() error = %v, want nil", err)
		}
	}
	return fixture
}

// started reports whether the fixture already ran the startup sequence.
func (f *fixture) started() bool { return f.server.TaskStore() != nil }

// newUnstartedFixture builds the server without running startup.
func newUnstartedFixture(t *testing.T, configure ...func(*fixtureOptions)) *fixture {
	t.Helper()

	var opts fixtureOptions
	for _, apply := range configure {
		apply(&opts)
	}
	if opts.model == nil {
		opts.model = &scriptedLLM{turns: [][]*adkmodel.LLMResponse{textTurn("Acknowledged.")}}
	}

	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.CopyFS(dataDir, os.DirFS(repositoryDataset)); err != nil {
		t.Fatalf("copy the committed dataset: %v", err)
	}
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		t.Fatalf("create the state directory: %v", err)
	}

	store := data.New(data.Config{DataDir: dataDir, StateDir: stateDir})
	sessions := opts.sessions
	if sessions == nil {
		sessions = newSessionService(t, stateDir)
	}
	served := opts.agent
	if opts.agentFactory != nil {
		served = opts.agentFactory(opts.model, store)
	} else if served == nil {
		served = newAgent(t, opts.model)
	}

	// The server's own diagnostics go to a discarded handler: a test asserts on
	// wire responses and stored state, and a warning about a deliberately broken
	// database is expected output, not a failure.
	steps := newRecordedSteps()
	options := a2aserver.Options{StateDir: stateDir, Entrypoint: config.EntrypointAgent}
	if opts.options != nil {
		opts.options(&options)
	}
	var promptGuardReadiness a2aserver.Step
	if opts.promptGuard != nil {
		promptGuardReadiness = func(context.Context) error { return nil }
	}
	if opts.promptGuardReadiness != nil {
		promptGuardReadiness = opts.promptGuardReadiness
	}

	server, err := a2aserver.New(a2aserver.Config{
		RootAgent:            served,
		SessionService:       sessions,
		Logger:               slog.New(slog.DiscardHandler),
		PromptGuardHandler:   opts.promptGuard,
		PromptGuardReadiness: promptGuardReadiness,
		// The real recovery, not a stub: its position in the sequence is the
		// course invariant this suite exists to pin.
		RecoverState: steps.step("recover", func(context.Context) error {
			return state.RecoverInterruptedRestore(stateDir, state.RecoverOptions{})
		}),
		PrepareDataset: steps.step("prepare", func(ctx context.Context) error {
			_, prepareErr := store.PrepareRuntimeDatabase(ctx)
			return prepareErr
		}),
		MigrateSessions: steps.step("migrate", func(context.Context) error {
			return database.AutoMigrate(sessions)
		}),
		ProbeDataset: func(ctx context.Context) error {
			_, probeErr := store.ProbeRuntimeDatabase(ctx)
			return probeErr
		},
		ProbeSessions: opts.probeSessions,
		Plugins:       opts.plugins,
		Options:       options,
	})
	if err != nil {
		t.Fatalf("a2aserver.New() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	return &fixture{server: server, model: opts.model, store: store, steps: steps, stateDir: stateDir}
}

// onSharedSessionBackend configures a fixture whose sessions live on a
// PostgreSQL server rather than in its state directory, with probe standing in
// for the check cmd/agent supplies over the pool it owns.
func onSharedSessionBackend(probe a2aserver.Step) func(*fixtureOptions) {
	return func(opts *fixtureOptions) {
		opts.options = func(options *a2aserver.Options) {
			options.SessionBackend = config.SessionBackendPostgres
		}
		opts.probeSessions = probe
	}
}

// newSessionService opens a real ADK session store on the fixture's state dir.
func newSessionService(t *testing.T, stateDir string) session.Service {
	t.Helper()

	sessions, err := database.NewSessionService(sqlite.Open(a2aserver.SessionDataSourceName(stateDir)))
	if err != nil {
		t.Fatalf("opening the session store: %v", err)
	}
	return sessions
}

// newAgent builds the smallest real llmagent over the scripted model.
func newAgent(t *testing.T, model *scriptedLLM, tools ...toolOption) agent.Agent {
	t.Helper()

	cfg := llmagent.Config{
		Name:        "a2a_test_agent",
		Description: "Answers with the scripted model.",
		Instruction: "Operating rules: reply using the scripted model.",
		Model:       model,
	}
	for _, apply := range tools {
		apply(&cfg)
	}
	built, err := llmagent.New(cfg)
	if err != nil {
		t.Fatalf("llmagent.New() error = %v, want nil", err)
	}
	return built
}

// toolOption adjusts the test agent's configuration.
type toolOption func(*llmagent.Config)

// serve starts an httptest server over the fixture's handler.
func (f *fixture) serve(t *testing.T) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(f.server.Handler())
	t.Cleanup(server.Close)
	return server
}

// rpc posts one JSON-RPC request and decodes the response envelope.
func rpc(t *testing.T, server *httptest.Server, path, method string, params any, headers ...[2]string) map[string]any {
	t.Helper()

	response := post(t, server, path, method, params, headers...)
	defer func() { _ = response.Body.Close() }()
	var envelope map[string]any
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatalf("decoding the %s response: %v", method, err)
	}
	return envelope
}

// post sends one JSON-RPC request and returns the live response.
func post(t *testing.T, server *httptest.Server, path, method string, params any, headers ...[2]string) *http.Response {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      method + "-test",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		t.Fatalf("encoding the %s request: %v", method, err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building the %s request: %v", method, err)
	}
	request.Header.Set("Content-Type", "application/json")
	for _, header := range headers {
		request.Header.Add(header[0], header[1])
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("sending the %s request: %v", method, err)
	}
	return response
}

// streamResults posts a streaming turn to the server root — the path the
// checked-in browser client uses — and returns every SSE result payload.
func streamResults(t *testing.T, server *httptest.Server, params any) []map[string]any {
	t.Helper()

	return streamMethod(t, server, a2aserver.RootPath, "message/stream", params)
}

// streamMethod posts a streaming request and returns every SSE result payload.
//
// The frames are read whole rather than collected from an iterator: this is a
// wire-level assertion about what a browser client receives, and the two
// protocol bindings name the same operation differently — "message/stream" on
// 0.3, "SendStreamingMessage" on 1.0.
func streamMethod(
	t *testing.T, server *httptest.Server, path, method string, params any, headers ...[2]string,
) []map[string]any {
	t.Helper()

	response := post(t, server, path, method, params, headers...)
	defer func() { _ = response.Body.Close() }()
	if got := response.StatusCode; got != http.StatusOK {
		t.Fatalf("%s status = %d, want %d", method, got, http.StatusOK)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("%s content type = %q, want text/event-stream", method, got)
	}
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the event stream: %v", err)
	}
	var results []map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		payload, found := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !found {
			continue
		}
		var envelope struct {
			Result map[string]any `json:"result"`
			Error  map[string]any `json:"error"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &envelope); err != nil {
			t.Fatalf("decoding an event frame: %v", err)
		}
		if envelope.Error != nil {
			t.Fatalf("event stream carried a JSON-RPC error: %v", envelope.Error)
		}
		results = append(results, envelope.Result)
	}
	return results
}

// liveStream is one open server-sent event connection, read one frame at a
// time.
//
// streamMethod answers "what did the client receive in the end", which is the
// wrong question for a Cancel button: that button is enabled or greyed out from
// what the client knows while the turn is still running. next blocks until the
// next frame arrives and returns nil once the server closes the stream.
type liveStream struct {
	next  func() map[string]any
	close func()
}

// streamRequest is one streaming call, as a value so [withStream] can keep the
// connection's lifetime in one function rather than in its signature.
type streamRequest struct {
	params  any
	path    string
	method  string
	headers [][2]string
}

// withStream posts a streaming request and hands the still-open stream to use.
//
// The connection closes when use returns; calling stream.close inside use hangs
// up first, which is how a browser tab closing mid-turn is reproduced. Closing
// an HTTP response body twice is safe, so both endings can coexist.
func withStream(t *testing.T, server *httptest.Server, request streamRequest, use func(stream *liveStream)) {
	t.Helper()

	response := post(t, server, request.path, request.method, request.params, request.headers...)
	defer func() { _ = response.Body.Close() }()
	if got := response.StatusCode; got != http.StatusOK {
		t.Fatalf("%s status = %d, want %d", request.method, got, http.StatusOK)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("%s content type = %q, want text/event-stream", request.method, got)
	}
	reader := bufio.NewReader(response.Body)
	use(&liveStream{
		close: func() { _ = response.Body.Close() },
		next: func() map[string]any {
			for {
				line, err := reader.ReadString('\n')
				// The line is decoded before the error is honored: the last frame
				// of a stream can arrive together with io.EOF.
				if payload, found := strings.CutPrefix(strings.TrimSpace(line), "data:"); found {
					var envelope struct {
						Result map[string]any `json:"result"`
						Error  map[string]any `json:"error"`
					}
					if decodeErr := json.Unmarshal([]byte(strings.TrimSpace(payload)), &envelope); decodeErr != nil {
						t.Errorf("decoding an event frame: %v", decodeErr)
						return nil
					}
					if envelope.Error != nil {
						t.Errorf("event stream carried a JSON-RPC error: %v", envelope.Error)
						return nil
					}
					return envelope.Result
				}
				if err != nil {
					return nil
				}
			}
		},
	})
}

// textMessage builds the params of a plain user turn.
func textMessage(id, text string) map[string]any {
	return map[string]any{"message": map[string]any{
		"kind":      "message",
		"messageId": id,
		"role":      "user",
		"parts":     []any{map[string]any{"kind": "text", "text": text}},
	}}
}

// kinds renders the event kinds of a stream, which is the shape assertion the
// Python suite makes first.
func kinds(results []map[string]any) []string {
	found := make([]string, 0, len(results))
	for _, result := range results {
		kind, _ := result["kind"].(string)
		found = append(found, kind)
	}
	return found
}

// states renders the task state of every status-carrying event.
func states(results []map[string]any) []string {
	var found []string
	for _, result := range results {
		status, ok := result["status"].(map[string]any)
		if !ok {
			continue
		}
		state, _ := status["state"].(string)
		found = append(found, state)
	}
	return found
}

// errIsNot fails when err does not match the expected sentinel.
func errIsNot(t *testing.T, err, want error, context string) {
	t.Helper()

	if !errors.Is(err, want) {
		t.Fatalf("%s error = %v, want %v", context, err, want)
	}
}

// readinessProblems asks for readiness and returns the reported problems.
func readinessProblems(t *testing.T, server *httptest.Server) []string {
	t.Helper()

	response, err := server.Client().Get(server.URL + a2aserver.ReadinessPath)
	if err != nil {
		t.Fatalf("GET %s: %v", a2aserver.ReadinessPath, err)
	}
	defer func() { _ = response.Body.Close() }()
	if got := response.StatusCode; got != http.StatusServiceUnavailable {
		t.Fatalf("readiness status = %d, want %d", got, http.StatusServiceUnavailable)
	}
	var body struct {
		Status   string   `json:"status"`
		Problems []string `json:"problems"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decoding the readiness body: %v", err)
	}
	if body.Status != "unready" {
		t.Errorf("readiness status = %q, want %q", body.Status, "unready")
	}
	return body.Problems
}

// openRaw opens a second SQLite handle on a database the server also holds.
//
// It is how a test breaks or observes state the way an operator would: from
// outside the process's own pool.
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()

	pool, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Errorf("closing %s: %v", path, err)
		}
	})
	return pool
}

// execSQL runs one statement against a database through its own connection.
func execSQL(t *testing.T, path, statement string) {
	t.Helper()

	if _, err := openRaw(t, path).ExecContext(t.Context(), statement); err != nil {
		t.Fatalf("running %q on %s: %v", statement, path, err)
	}
}

// writeFutureTaskStore creates a task database carrying only a schema marker.
//
// With an unknown version it is the "state written by a newer binary" case;
// with the current version it is the "current version, incomplete structure"
// case. Neither can be produced by this binary, which is the point.
func writeFutureTaskStore(t *testing.T, path, version string) {
	t.Helper()

	pool := openRaw(t, path)
	if _, err := pool.ExecContext(t.Context(),
		`CREATE TABLE store_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("creating the marker table: %v", err)
	}
	if _, err := pool.ExecContext(t.Context(),
		`INSERT INTO store_metadata (key, value) VALUES ('schema_version', ?)`, version); err != nil {
		t.Fatalf("stamping the marker: %v", err)
	}
}

// holdTaskStoreConnection keeps a write transaction open on the task database
// while body runs — a second writer, exactly as another replica or an operator
// would be.
func holdTaskStoreConnection(t *testing.T, fixture *fixture, body func()) {
	t.Helper()

	pool := openRaw(t, filepath.Join(fixture.stateDir, a2aserver.TaskDatabaseName))
	tx, err := pool.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("beginning a competing transaction: %v", err)
	}
	body()
	if err := tx.Rollback(); err != nil {
		t.Errorf("rolling back the competing transaction: %v", err)
	}
}

// sessionRowCount counts the ADK session rows for one A2A context id.
//
// It reads the session database directly because the question is about what
// the store holds, not about what the API reports — and a duplicate row is
// exactly the failure the recovery exists to prevent.
func sessionRowCount(t *testing.T, fixture *fixture, contextID string) int {
	t.Helper()

	pool := openRaw(t, filepath.Join(fixture.stateDir, a2aserver.SessionDatabaseName))
	var count int
	if err := pool.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM sessions WHERE id = ?`, contextID).Scan(&count); err != nil {
		t.Fatalf("counting session rows: %v", err)
	}
	return count
}

// readFile returns a file's bytes, so a test can prove a refusal changed
// nothing.
func readFile(t *testing.T, path string) []byte {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return content
}
