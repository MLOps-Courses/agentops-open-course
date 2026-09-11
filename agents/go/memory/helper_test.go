package memory

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"hash/crc32"
	"iter"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	// The pure-Go SQLite driver, so a fixture can inspect and corrupt the two
	// databases without going through the code under test. A fixture that used
	// the production connection could not contradict it.
	_ "github.com/glebarez/go-sqlite"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/data"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// repositoryDataset is the committed dataset, relative to this package. Every
// test copies it before touching it: the committed incidents.db is
// byte-reproducible and gated by a rebuild comparison, so a test that wrote to
// it would break the repository rather than only itself.
const repositoryDataset = "../../data"

// Identifiers come from the vocabulary rather than from literals here, so a
// pivoted domain carries the suite with it — and so the ratchet in
// domain/portability_test.go stays green.
var (
	inventoryService  = domain.Reference().Services.Inventory
	checkoutIncident  = domain.Reference().Incidents.CheckoutLatency
	inventoryIncident = domain.Reference().Incidents.InventoryDown
	serviceDownBook   = domain.Reference().Runbooks.ServiceDown
	highLatencyBook   = domain.Reference().Runbooks.HighLatency
	diskFullBook      = domain.Reference().Runbooks.DiskFull
	cascadeBook       = domain.Reference().Runbooks.CascadeFailure
)

// The three embedding models the suite configures. Only the first is the
// reference default; the other two exist so a model swap can be observed
// without also changing what the fake endpoint knows about.
const (
	embeddingModel   = "nomic-embed-text"
	replacementModel = "replacement-embed"
	brokenModel      = "broken-embed"
)

// fakeDimensions is the width of the fake embedder's vectors: wide enough that
// two runbooks rarely collide, narrow enough to read in a failure message.
const fakeDimensions = 32

// The identity every scoped test calls with.
const (
	testApp       = "agentops-agent"
	testUser      = "engineer"
	testSessionID = "session-7"
)

// options tunes a fixture the way the Python suite tuned its settings fixture.
type options struct {
	// redact replaces the default identity redactor. A test that supplies one is
	// asserting something about the seam, not about any particular redaction.
	redact Redactor
	// store replaces the real store, for the failures a real one cannot be made
	// to produce on demand.
	store func(Store) Store
	// configure adjusts the fake embeddings endpoint before the memory is built.
	configure func(*ollama)
	// embeddingModel selects which model name the configuration asks for. Empty
	// uses the reference default.
	embeddingModel string
	// embeddingTimeout bounds one embedding request. Zero leaves it unbounded,
	// which is what every test but the deadline one wants.
	embeddingTimeout time.Duration
	// Field order below is what go vet's fieldalignment check wants.
	semanticRetrieval bool
	writesDisabled    bool
}

// fixture is one isolated dataset and state directory, a fake embeddings
// endpoint, and the memory surface built over them.
type fixture struct {
	memory *Memory
	store  *data.Store
	ollama *ollama
	guard  *recordingGuard
	// logs captures the fallback warning, which is the one event this package
	// logs and the one the Python suite asserted with caplog.
	logs *bytes.Buffer
	// redacted records every text the redactor was handed, which is how the
	// suite proves the redaction seam is on the persistence path.
	redacted *recorder

	dataDir  string
	stateDir string
}

// newFixture copies the committed dataset into a temporary directory, starts a
// fake embeddings endpoint, and builds the memory surface over both.
func newFixture(t *testing.T, configure ...func(*options)) *fixture {
	t.Helper()

	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.CopyFS(dataDir, os.DirFS(repositoryDataset)); err != nil {
		t.Fatalf("copy the committed dataset: %v", err)
	}
	built := &fixture{
		dataDir:  dataDir,
		stateDir: filepath.Join(root, "state"),
		ollama:   newOllama(t),
	}
	built.reconfigure(t, configure...)
	return built
}

// reconfigure rebuilds the memory surface over the same dataset, state
// directory and embeddings endpoint.
//
// It is how this suite expresses "the operator changed a setting and restarted
// the process", which is what the Python tests did by monkeypatching the
// settings singleton. Nothing on disk is reset, so a generation built before
// the change is still there to be judged stale.
func (f *fixture) reconfigure(t *testing.T, configure ...func(*options)) {
	t.Helper()

	var opts options
	for _, apply := range configure {
		apply(&opts)
	}
	if opts.configure != nil {
		opts.configure(f.ollama)
	}
	model := opts.embeddingModel
	if model == "" {
		model = embeddingModel
	}
	f.store = data.New(data.Config{DataDir: f.dataDir, StateDir: f.stateDir})

	var storeUnderTest Store = f.store
	if opts.store != nil {
		storeUnderTest = opts.store(f.store)
	}
	redact := opts.redact
	if redact == nil {
		redact = func(text string) string { return text }
	}
	f.redacted = &recorder{}
	f.guard = &recordingGuard{}
	f.logs = &bytes.Buffer{}

	built, err := New(Config{
		Store: storeUnderTest,
		Guard: f.guard.run,
		Redact: func(text string) string {
			f.redacted.record(text)
			return redact(text)
		},
		HTTPClient:        f.ollama.server.Client(),
		Logger:            slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
		StateDir:          f.stateDir,
		EmbeddingsURL:     f.ollama.server.URL,
		EmbeddingModel:    model,
		EmbeddingTimeout:  opts.embeddingTimeout,
		SemanticRetrieval: opts.semanticRetrieval,
		WritesDisabled:    opts.writesDisabled,
	})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	f.memory = built
}

// runbookPath returns one runbook file inside the fixture's private copy of the
// dataset, so a test can edit the corpus without touching the repository.
func (f *fixture) runbookPath(slug string) string {
	return filepath.Join(f.dataDir, "runbooks", slug+".md")
}

// appendToRunbook grows one runbook, which changes the corpus hash and must
// therefore invalidate the whole vector generation.
func (f *fixture) appendToRunbook(t *testing.T, slug, addition string) {
	t.Helper()

	path := f.runbookPath(slug)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := os.WriteFile(path, append(content, []byte(addition)...), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// recorder collects strings from any number of goroutines. A fixture shared by
// parallel subtests is only usable if the things it observes are safe to
// observe concurrently.
type recorder struct {
	values []string
	mu     sync.Mutex
}

func (r *recorder) record(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.values = append(r.values, value)
}

// all returns a copy, so a caller reading it cannot race a later write.
func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.values...)
}

// recordingGuard is a [Guard] that runs each call once and remembers which
// tools it wrapped, so a test can prove the reads go through the resilience
// seam and the write does not.
type recordingGuard struct {
	// before runs immediately before the call and may replace it entirely by
	// returning an error, which is how a failing guard is simulated.
	before func(name string) error
	// wrap derives the context one attempt runs under, standing in for the
	// per-attempt deadline the resilience Guard installs. A test uses it to hand a
	// read a budget that is already gone, which is the only way to observe what a
	// tool does with a context it cannot get more time from.
	wrap  func(ctx context.Context) context.Context
	names recorder
}

func (g *recordingGuard) run(ctx context.Context, name string, call func(context.Context) error) error {
	g.names.record(name)
	if g.before != nil {
		if err := g.before(name); err != nil {
			return err
		}
	}
	if g.wrap != nil {
		ctx = g.wrap(ctx)
	}
	return call(ctx)
}

// ollama is the fake local embeddings endpoint.
//
// Every test in this package is offline: nothing here opens a socket outside
// the test process, so the suite is the same on a laptop with a model pulled
// and in CI without one.
//
// Field order below is what go vet's fieldalignment check wants, not what a
// reader would group.
type ollama struct {
	server *httptest.Server
	// embed replaces the deterministic embedder when set.
	embed func(texts []string) ([][]float64, error)
	// embedBody, when non-empty, is written verbatim instead of real vectors.
	embedBody string
	// tagsBody, when non-empty, is written verbatim instead of a model list.
	tagsBody string
	// batches records the input of every /api/embed call, so a test can prove
	// how many times the corpus was re-embedded.
	batches [][]string
	// digests is the sequence of digests /api/tags reports; the last one repeats
	// for every later call. A one-element sequence is a stable model.
	digests []string
	// models are the names /api/tags advertises, each rendered with a :latest tag.
	models []string
	// embedDelay sleeps before answering /api/embed, for the deadline test.
	embedDelay time.Duration
	mu         sync.Mutex
	// tagCalls counts /api/tags requests.
	tagCalls int
	// dimensions is the width of every produced vector.
	dimensions int
	// embedStatus overrides the /api/embed status code.
	embedStatus int
}

func newOllama(t *testing.T) *ollama {
	t.Helper()

	fake := &ollama{
		digests:    []string{"sha256:model-a"},
		models:     []string{embeddingModel, replacementModel, brokenModel},
		dimensions: fakeDimensions,
	}
	mux := http.NewServeMux()
	mux.HandleFunc(embedPath, fake.handleEmbed)
	mux.HandleFunc(tagsPath, fake.handleTags)
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

func (o *ollama) handleEmbed(writer http.ResponseWriter, request *http.Request) {
	var payload struct {
		Input []string `json:"input"`
	}
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	o.mu.Lock()
	o.batches = append(o.batches, payload.Input)
	delay, status, body, embed, dimensions := o.embedDelay, o.embedStatus, o.embedBody, o.embed, o.dimensions
	o.mu.Unlock()

	if delay > 0 {
		time.Sleep(delay)
	}
	if status != 0 && status != http.StatusOK {
		writer.WriteHeader(status)
		return
	}
	if body != "" {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(body))
		return
	}
	vectors := make([][]float64, 0, len(payload.Input))
	if embed != nil {
		produced, err := embed(payload.Input)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusInternalServerError)
			return
		}
		vectors = produced
	} else {
		for _, text := range payload.Input {
			vectors = append(vectors, fakeVector(text, dimensions))
		}
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"embeddings": vectors})
}

func (o *ollama) handleTags(writer http.ResponseWriter, _ *http.Request) {
	o.mu.Lock()
	index := min(o.tagCalls, len(o.digests)-1)
	o.tagCalls++
	digest := ""
	if index >= 0 {
		digest = o.digests[index]
	}
	body, models := o.tagsBody, append([]string(nil), o.models...)
	o.mu.Unlock()

	writer.Header().Set("Content-Type", "application/json")
	if body != "" {
		_, _ = writer.Write([]byte(body))
		return
	}
	entries := make([]map[string]string, 0, len(models)+1)
	// A model the configuration never asks for, so the alias match is doing real
	// work rather than accepting whatever came first.
	entries = append(entries, map[string]string{"name": "another-model:latest", "digest": "sha256:other"})
	for _, name := range models {
		entries = append(entries, map[string]string{"name": name + ":latest", "digest": digest})
	}
	_ = json.NewEncoder(writer).Encode(map[string]any{"models": entries})
}

// indexBatches returns the /api/embed calls that carried more than one text —
// the corpus embeddings. A query embeds exactly one text, so the distinction is
// exact and needs no other bookkeeping.
func (o *ollama) indexBatches() int {
	o.mu.Lock()
	defer o.mu.Unlock()

	count := 0
	for _, batch := range o.batches {
		if len(batch) > 1 {
			count++
		}
	}
	return count
}

// queryBatches returns the /api/embed calls that carried exactly one text.
func (o *ollama) queryBatches() [][]string {
	o.mu.Lock()
	defer o.mu.Unlock()

	single := make([][]string, 0, len(o.batches))
	for _, batch := range o.batches {
		if len(batch) == 1 {
			single = append(single, batch)
		}
	}
	return single
}

// setDigests replaces the digest sequence /api/tags walks through.
func (o *ollama) setDigests(digests ...string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.digests = digests
	o.tagCalls = 0
}

// fakeVector is a deterministic bag-of-words projection: texts sharing words
// land near each other, which is enough to exercise the ranking plumbing
// without a model. It is the same construction the Python suite used, down to
// the CRC-32 bucket, so both tracks rank the corpus identically.
func fakeVector(text string, dimensions int) []float64 {
	vector := make([]float64, dimensions)
	for _, token := range strings.Fields(strings.ToLower(text)) {
		vector[crc32.ChecksumIEEE([]byte(token))%uint32(dimensions)]++
	}
	norm := 0.0
	for _, value := range vector {
		norm += value * value
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		norm = 1
	}
	for index := range vector {
		vector[index] /= norm
	}
	return vector
}

// runnable is the structural interface every ADK function tool satisfies. The
// declaring interface lives in an internal package, so a caller outside the ADK
// module has to name the method set itself.
type runnable interface {
	Run(ctx agent.Context, args any) (map[string]any, error)
}

// declared exposes the declaration ADK sends to the model.
type declared interface {
	Declaration() *genai.FunctionDeclaration
}

// run calls a tool exactly the way the ADK flow calls it, and returns the map
// that would reach the model.
func run(t *testing.T, target tool.Tool, ctx agent.Context, args map[string]any) (map[string]any, error) {
	t.Helper()

	callable, ok := target.(runnable)
	if !ok {
		t.Fatalf("tool %q does not expose Run", target.Name())
	}
	return callable.Run(ctx, args)
}

// mustRun runs a tool that is expected to answer rather than fail, and decodes
// its result back from the wire shape ADK would send.
//
// Asserting through that shape rather than on the handler's return value is
// what makes the claims about it — the omitted keys especially — mean anything.
func mustRun[T any](t *testing.T, target tool.Tool, ctx agent.Context, args map[string]any) T {
	t.Helper()

	result, err := run(t, target, ctx, args)
	if err != nil {
		t.Fatalf("%s.Run() error = %v, want nil", target.Name(), err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("re-encode the tool result: %v", err)
	}
	var decoded T
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode the tool result %s: %v", encoded, err)
	}
	return decoded
}

// toolContext builds the agent.Context a tool sees for one identity.
//
// ADK's own tests build one from internal/context, which is un-importable from
// outside the module, so the invocation context is supplied here instead.
func toolContext(t *testing.T, appName, userID, sessionID string) agent.Context {
	t.Helper()

	invocation := &fakeInvocation{
		Context: t.Context(),
		session: fakeSession{id: sessionID, appName: appName, userID: userID},
	}
	return agent.NewToolContext(invocation, "function-call-1", &session.EventActions{}, nil)
}

// engineerContext is the common case: the reference application, one named
// engineer, one session.
func engineerContext(t *testing.T) agent.Context {
	t.Helper()

	return toolContext(t, testApp, testUser, testSessionID)
}

// fakeSession is the smallest session a tool context reads: an id, an app name
// and a user id. The state and event accessors are never reached from a tool,
// and returning nil makes that loud rather than silently plausible.
type fakeSession struct {
	events  session.Events
	updated time.Time
	id      string
	appName string
	userID  string
}

func (s fakeSession) ID() string                { return s.id }
func (s fakeSession) AppName() string           { return s.appName }
func (s fakeSession) UserID() string            { return s.userID }
func (s fakeSession) State() session.State      { return nil }
func (s fakeSession) Events() session.Events    { return s.events }
func (s fakeSession) LastUpdateTime() time.Time { return s.updated }

// fakeInvocation is the minimum agent.InvocationContext a tool context needs.
// Everything a tool actually reads is real; the rest reports a zero value.
type fakeInvocation struct {
	context.Context
	session      session.Session
	memory       agent.Memory
	userContent  *genai.Content
	invocationID string
}

func (i *fakeInvocation) Agent() agent.Agent              { return nil }
func (i *fakeInvocation) Artifacts() agent.Artifacts      { return nil }
func (i *fakeInvocation) Memory() agent.Memory            { return i.memory }
func (i *fakeInvocation) Session() session.Session        { return i.session }
func (i *fakeInvocation) InvocationID() string            { return i.invocationID }
func (i *fakeInvocation) Branch() string                  { return "" }
func (i *fakeInvocation) IsolationScope() string          { return "" }
func (i *fakeInvocation) UserContent() *genai.Content     { return i.userContent }
func (i *fakeInvocation) RunConfig() *agent.RunConfig     { return nil }
func (i *fakeInvocation) EndInvocation()                  {}
func (i *fakeInvocation) Ended() bool                     { return false }
func (i *fakeInvocation) ResumedInput(string) (any, bool) { return nil, false }

func (i *fakeInvocation) WithContext(ctx context.Context) agent.InvocationContext {
	replaced := *i
	replaced.Context = ctx
	return &replaced
}

func (i *fakeInvocation) WithICDelta(*agent.InvocationContextDelta) agent.InvocationContext { return i }

// eventList is the smallest session.Events a memory ingest reads.
type eventList []*session.Event

func (e eventList) All() iter.Seq[*session.Event] {
	return func(yield func(*session.Event) bool) {
		for _, event := range e {
			if !yield(event) {
				return
			}
		}
	}
}

func (e eventList) Len() int                    { return len(e) }
func (e eventList) At(index int) *session.Event { return e[index] }

// stubStore delegates to a real store and overrides one operation, which is the
// only way to reach the branches a real dataset cannot be made to take.
type stubStore struct {
	Store
	listRunbookSlugs func() ([]string, error)
	readRunbook      func(slug string) (string, bool, error)
	getIncident      func() (*domain.Incident, error)
}

func (s stubStore) ListRunbookSlugs() ([]string, error) {
	if s.listRunbookSlugs != nil {
		return s.listRunbookSlugs()
	}
	return s.Store.ListRunbookSlugs()
}

func (s stubStore) ReadRunbook(slug string) (string, bool, error) {
	if s.readRunbook != nil {
		return s.readRunbook(slug)
	}
	return s.Store.ReadRunbook(slug)
}

func (s stubStore) GetIncident(ctx context.Context, id domain.IncidentID) (*domain.Incident, error) {
	if s.getIncident != nil {
		return s.getIncident()
	}
	return s.Store.GetIncident(ctx, id)
}

// contains fails unless haystack contains needle, reporting both.
func contains(t *testing.T, haystack, needle, what string) {
	t.Helper()

	if !strings.Contains(haystack, needle) {
		t.Errorf("%s = %q, want it to contain %q", what, haystack, needle)
	}
}

// unavailable asserts a failure is the one search_runbooks recovers from, and
// returns its message.
func unavailable(t *testing.T, err error) string {
	t.Helper()

	var target *EmbeddingUnavailableError
	if !errors.As(err, &target) {
		t.Fatalf("error = %v, want an *EmbeddingUnavailableError", err)
	}
	return target.Error()
}

// pointer is the shorthand the option tables use to say "set this, possibly to
// the zero value".
func pointer[T any](value T) *T { return &value }

// errAssertion stands in for a dependency that simply failed.
var errAssertion = errors.New("the dependency is unavailable")

// storedNotes reads the note text straight out of the memory database, outside
// the code under test. A fixture that asked the store what it stored could not
// contradict it.
func storedNotes(t *testing.T, f *fixture) []string {
	t.Helper()

	path := filepath.Join(f.stateDir, notesDatabaseName)
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("close %s: %v", path, closeErr)
		}
	})
	rows, err := db.QueryContext(t.Context(), "SELECT note FROM incident_notes ORDER BY id")
	if err != nil {
		t.Fatalf("read the notes: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var notes []string
	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			t.Fatalf("scan a note: %v", err)
		}
		notes = append(notes, note)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the notes: %v", err)
	}
	return notes
}

// update mutates the fake endpoint under its own lock, so a test can change how
// it answers while a search is in flight.
func (o *ollama) update(mutate func(*ollama)) {
	o.mu.Lock()
	defer o.mu.Unlock()

	mutate(o)
}

// vectorsDB opens the vector index directly, for the assertions the retriever
// has no API for — the storage type of an embedding, or a deleted metadata row.
func vectorsDB(t *testing.T, f *fixture) *sql.DB {
	t.Helper()

	path := filepath.Join(f.stateDir, vectorsDatabaseName)
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("close %s: %v", path, closeErr)
		}
	})
	return db
}

// closedServer is an endpoint that is definitely not listening, without any
// traffic ever leaving the test process.
func closedServer(t *testing.T) string {
	t.Helper()

	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()
	return url
}
