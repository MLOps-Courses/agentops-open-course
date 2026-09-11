package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	// The pure-Go SQLite driver, so a fixture can inspect and corrupt the runtime
	// database without going through the code under test. A fixture that used the
	// production connection could not contradict it.
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
	checkoutService   = domain.Reference().Services.Checkout
	inventoryService  = domain.Reference().Services.Inventory
	paymentsService   = domain.Reference().Services.Payments
	checkoutIncident  = domain.Reference().Incidents.CheckoutLatency
	inventoryIncident = domain.Reference().Incidents.InventoryDown
	resolvedIncident  = domain.Reference().Incidents.PaymentsErrors
	highLatencyBook   = domain.Reference().Runbooks.HighLatency
)

// The approval identity every guarded-write test confirms with.
const (
	testApprover     = "engineer"
	testSessionID    = "session-7"
	testInvocationID = "invocation-9"
)

// fixture is one isolated dataset plus the tools built over it.
type fixture struct {
	tools *Tools
	store *data.Store
	guard *recordingGuard
	// redacted records every rationale the redactor was handed, which is how the
	// suite proves the redaction seam is actually on the persistence path.
	redacted []string
}

// options tunes a fixture the way the Python suite tuned its settings fixture.
type options struct {
	// redact replaces the default identity redactor. A test that supplies one is
	// asserting something about the seam, not about any particular redaction.
	redact Redactor
	// store replaces the real store, for the two races a real one cannot be made
	// to lose on demand.
	store func(Store) Store
	// Field order here, and in the structs below, is what go vet's fieldalignment
	// check wants: Go lays a struct out in declaration order, so the fields that
	// carry pointers come first.
	writesDisabled bool
}

// newFixture copies the committed dataset into a temporary directory and builds
// the tools over it.
func newFixture(t *testing.T, configure ...func(*options)) *fixture {
	t.Helper()

	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.CopyFS(dataDir, os.DirFS(repositoryDataset)); err != nil {
		t.Fatalf("copy the committed dataset: %v", err)
	}
	store := data.New(data.Config{DataDir: dataDir, StateDir: filepath.Join(root, "state")})

	var opts options
	for _, apply := range configure {
		apply(&opts)
	}

	fixture := &fixture{store: store, guard: &recordingGuard{}}
	var storeUnderTest Store = store
	if opts.store != nil {
		storeUnderTest = opts.store(store)
	}
	redact := opts.redact
	if redact == nil {
		redact = func(text string) string { return text }
	}

	built, err := New(Config{
		Store: storeUnderTest,
		Guard: fixture.guard.run,
		Redact: func(text string) string {
			fixture.redacted = append(fixture.redacted, text)
			return redact(text)
		},
		WritesDisabled: opts.writesDisabled,
	})
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	fixture.tools = built
	return fixture
}

// recordingGuard is a [Guard] that runs each call once and remembers which
// tools it wrapped, so a test can prove the reads go through the resilience
// seam and the guarded writes do not.
type recordingGuard struct {
	// before runs immediately before the call, and may replace it entirely by
	// returning an error, which is how a failing guard is simulated.
	before func(name string) error
	// wrap derives the context one attempt runs under, standing in for the
	// per-attempt deadline the resilience Guard installs. A test uses it to hand a
	// read a budget that is already gone, which is the only way to observe what a
	// tool does with a context it cannot get more time from.
	wrap  func(ctx context.Context) context.Context
	names []string
	// repeat runs the call this many extra times, standing in for the retries the
	// resilience package performs.
	repeat int
}

func (g *recordingGuard) run(ctx context.Context, name string, call func(context.Context) error) error {
	g.names = append(g.names, name)
	if g.before != nil {
		if err := g.before(name); err != nil {
			return err
		}
	}
	for attempt := 0; attempt <= g.repeat; attempt++ {
		attemptCtx := ctx
		if g.wrap != nil {
			attemptCtx = g.wrap(ctx)
		}
		if err := call(attemptCtx); err != nil {
			return err
		}
	}
	return nil
}

// serviceStatus reads a service's current status straight from the store.
func (f *fixture) serviceStatus(t *testing.T, name string) domain.ServiceStatus {
	t.Helper()

	service, err := f.store.GetService(t.Context(), mustSlug(t, name))
	if err != nil {
		t.Fatalf("GetService(%q) error = %v, want nil", name, err)
	}
	if service == nil {
		t.Fatalf("GetService(%q) = nil, want a service", name)
	}
	return service.Status()
}

// incidentStatus reads an incident's current status straight from the store.
func (f *fixture) incidentStatus(t *testing.T, id string) domain.IncidentStatus {
	t.Helper()

	incident, err := f.store.GetIncident(t.Context(), mustIncidentID(t, id))
	if err != nil {
		t.Fatalf("GetIncident(%q) error = %v, want nil", id, err)
	}
	if incident == nil {
		t.Fatalf("GetIncident(%q) = nil, want an incident", id)
	}
	return incident.Status()
}

// runtimeDB opens the published runtime database read-write, for assertions the
// tools have no API for — counting audit rows, or installing a hostile trigger.
func (f *fixture) runtimeDB(t *testing.T) *sql.DB {
	t.Helper()

	path := f.store.RuntimePath()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("runtime state has not been published yet: %v", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("resolve the runtime database path: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+absolute+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open the runtime database: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the runtime database: %v", err)
		}
	})
	return db
}

// auditCount counts the rows in the append-only trail.
func (f *fixture) auditCount(t *testing.T) int {
	t.Helper()

	var count int
	if err := f.runtimeDB(t).QueryRowContext(t.Context(), "SELECT COUNT(*) FROM audit_log").Scan(&count); err != nil {
		t.Fatalf("count the audit rows: %v", err)
	}
	return count
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
//
// A refusal is a result, so err is nil for every refusal in this suite; only a
// genuine failure and the two confirmation sentinels produce one.
func run(t *testing.T, target tool.Tool, ctx agent.Context, args map[string]any) (map[string]any, error) {
	t.Helper()

	callable, ok := target.(runnable)
	if !ok {
		t.Fatalf("tool %q does not expose Run", target.Name())
	}
	return callable.Run(ctx, args)
}

// decode re-reads the map ADK would send to the model back into the tool's own
// result type. Asserting through the wire shape rather than on the handler's
// return value is what makes the claims about that shape — the omitted keys
// especially — mean anything.
func decode[T any](t *testing.T, result map[string]any) T {
	t.Helper()

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

// mustRun runs a tool that is expected to answer rather than fail, and decodes
// its result.
func mustRun[T any](t *testing.T, target tool.Tool, ctx agent.Context, args map[string]any) T {
	t.Helper()

	result, err := run(t, target, ctx, args)
	if err != nil {
		t.Fatalf("%s.Run() error = %v, want nil", target.Name(), err)
	}
	return decode[T](t, result)
}

// confirmation describes the approval a tool context carries.
type confirmation struct {
	payload   any
	confirmed bool
	// absent builds a context with no confirmation at all, which is what the
	// first, unapproved delivery of a guarded call looks like.
	absent bool
}

// identity describes who the approval is attributable to. The zero value is the
// well-formed engineer every happy-path test approves as.
type identity struct {
	userID       *string
	sessionID    *string
	invocationID *string
}

// toolContext builds the agent.Context a tool sees, plus the EventActions the
// runtime would inspect afterwards.
//
// ADK's own tests build one from internal/context, which is un-importable from
// outside the module, so the invocation context is supplied here instead.
func toolContext(t *testing.T, approval confirmation, who identity) (agent.Context, *session.EventActions) {
	t.Helper()
	return toolContextWithBase(t, t.Context(), approval, who)
}

// toolContextWithBase is the boundary-aware variant used when a test must prove
// which request provenance reaches a guarded tool.
func toolContextWithBase(
	t *testing.T, base context.Context, approval confirmation, who identity,
) (agent.Context, *session.EventActions) {
	t.Helper()

	invocation := &fakeInvocation{
		Context: base,
		session: fakeSession{
			id:      valueOr(who.sessionID, testSessionID),
			appName: auditActor,
			userID:  valueOr(who.userID, testApprover),
		},
		invocationID: valueOr(who.invocationID, testInvocationID),
	}
	var confirmed *toolconfirmation.ToolConfirmation
	if !approval.absent {
		confirmed = &toolconfirmation.ToolConfirmation{
			Confirmed: approval.confirmed,
			Payload:   approval.payload,
		}
	}
	actions := &session.EventActions{}
	return agent.NewToolContext(invocation, "function-call-1", actions, confirmed), actions
}

// approvedContext is the common case: a confirmed approval carrying a rationale
// object, from a fully identified engineer.
func approvedContext(t *testing.T, rationale string) agent.Context {
	t.Helper()

	ctx, _ := toolContext(t, confirmation{
		confirmed: true,
		payload:   map[string]any{"rationale": rationale},
	}, identity{})
	return ctx
}

// valueOr resolves an override that must be able to express the empty string,
// which is exactly the case the identity checks refuse.
func valueOr(override *string, fallback string) string {
	if override == nil {
		return fallback
	}
	return *override
}

// pointer is the shorthand the option tables use to say "set this, possibly to
// the empty string".
func pointer[T any](value T) *T { return &value }

// fakeSession is the smallest session a tool context reads: an id, an app name
// and a user id. The state and event accessors are never reached from a tool,
// and returning nil makes that loud rather than silently plausible.
type fakeSession struct {
	id      string
	appName string
	userID  string
}

func (s fakeSession) ID() string                { return s.id }
func (s fakeSession) AppName() string           { return s.appName }
func (s fakeSession) UserID() string            { return s.userID }
func (s fakeSession) State() session.State      { return nil }
func (s fakeSession) Events() session.Events    { return nil }
func (s fakeSession) LastUpdateTime() time.Time { return time.Time{} }

// fakeInvocation is the minimum agent.InvocationContext a tool context needs.
// Everything a tool actually reads is real; the rest reports a zero value.
type fakeInvocation struct {
	context.Context
	session      session.Session
	invocationID string
}

func (i *fakeInvocation) Agent() agent.Agent              { return nil }
func (i *fakeInvocation) Artifacts() agent.Artifacts      { return nil }
func (i *fakeInvocation) Memory() agent.Memory            { return nil }
func (i *fakeInvocation) Session() session.Session        { return i.session }
func (i *fakeInvocation) InvocationID() string            { return i.invocationID }
func (i *fakeInvocation) Branch() string                  { return "" }
func (i *fakeInvocation) IsolationScope() string          { return "" }
func (i *fakeInvocation) UserContent() *genai.Content     { return nil }
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

// stubStore delegates to a real store and overrides one operation.
//
// It is the only way to reach two branches a real store cannot be made to take
// on demand: the races Python monkeypatched — a service removed, or an incident
// resolved, between the tool's own check and the transaction — and a dataset
// that is simply unreachable.
type stubStore struct {
	Store
	listIncidents func() ([]domain.Incident, error)
	getIncident   func() (*domain.Incident, error)
	listServices  func() ([]domain.Service, error)
	getService    func() (*domain.Service, error)
	readLogs      func() ([]string, bool, error)
	logsDir       func() string
	restart       func() (*domain.AuditEntry, error)
	resolve       func() (*domain.AuditEntry, error)
}

func (s stubStore) ListIncidents(ctx context.Context, filter data.IncidentFilter) ([]domain.Incident, error) {
	if s.listIncidents != nil {
		return s.listIncidents()
	}
	return s.Store.ListIncidents(ctx, filter)
}

func (s stubStore) GetIncident(ctx context.Context, id domain.IncidentID) (*domain.Incident, error) {
	if s.getIncident != nil {
		return s.getIncident()
	}
	return s.Store.GetIncident(ctx, id)
}

func (s stubStore) ListServices(ctx context.Context) ([]domain.Service, error) {
	if s.listServices != nil {
		return s.listServices()
	}
	return s.Store.ListServices(ctx)
}

func (s stubStore) GetService(ctx context.Context, name domain.Slug) (*domain.Service, error) {
	if s.getService != nil {
		return s.getService()
	}
	return s.Store.GetService(ctx, name)
}

func (s stubStore) ReadServiceLogs(service string) ([]string, bool, error) {
	if s.readLogs != nil {
		return s.readLogs()
	}
	return s.Store.ReadServiceLogs(service)
}

func (s stubStore) LogsDir() string {
	if s.logsDir != nil {
		return s.logsDir()
	}
	return s.Store.LogsDir()
}

func (s stubStore) RestartServiceWithAudit(
	ctx context.Context, name domain.Slug, audit data.AuditIdentity,
) (*domain.AuditEntry, error) {
	if s.restart != nil {
		return s.restart()
	}
	return s.Store.RestartServiceWithAudit(ctx, name, audit)
}

func (s stubStore) ResolveIncidentWithAudit(
	ctx context.Context, id domain.IncidentID, audit data.AuditIdentity,
) (*domain.AuditEntry, error) {
	if s.resolve != nil {
		return s.resolve()
	}
	return s.Store.ResolveIncidentWithAudit(ctx, id, audit)
}

func mustSlug(t *testing.T, value string) domain.Slug {
	t.Helper()

	slug, err := domain.ParseSlug(value)
	if err != nil {
		t.Fatalf("ParseSlug(%q) error = %v, want nil", value, err)
	}
	return slug
}

func mustIncidentID(t *testing.T, value string) domain.IncidentID {
	t.Helper()

	id, err := domain.ParseIncidentID(value)
	if err != nil {
		t.Fatalf("ParseIncidentID(%q) error = %v, want nil", value, err)
	}
	return id
}

// contains fails unless haystack contains needle, reporting both.
func contains(t *testing.T, haystack, needle, what string) {
	t.Helper()

	if !strings.Contains(haystack, needle) {
		t.Errorf("%s = %q, want it to contain %q", what, haystack, needle)
	}
}

// errUnavailable stands in for a dependency the guard could not reach.
var errUnavailable = errors.New("the dependency is unavailable")
