package a2aserver

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	adka2a "google.golang.org/adk/v2/server/adka2a/v2"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// CallBudgetPluginName identifies the per-invocation model-call bound to ADK's
// plugin manager, which refuses two plugins sharing a name.
const CallBudgetPluginName = "agentops_a2a_call_budget"

// ErrCallBudgetExceeded is what an invocation gets when it has asked the model
// for more turns than AGENT_A2A_MAX_LLM_CALLS allows.
//
// It is a sentinel so a caller can tell a bounded runaway apart from a model
// outage: the A2A task ends failed either way, but only one of them is a bug in
// the plan the model made.
var ErrCallBudgetExceeded = errors.New("model call budget exhausted")

// runnerProvider builds one ADK runner per A2A execution.
//
// It replaces adka2a's default provider for one reason: the returned runner is
// wrapped so that two overlapping invocations of the same logical session run
// one after the other. See [serializingRunner].
//
// The executor plugin ADK hands in must be installed or every callback that
// takes an adka2a.ExecutorContext stops working, so it is appended to a clone
// of the application's own plugin list — never to the shared slice.
func (s *Server) runnerProvider(
	_ context.Context, _ *a2asrv.ExecutorContext, executorPlugin *plugin.Plugin,
) (adka2a.RunnerConfig, adka2a.Runner, error) {
	plugins := append(slices.Clone(s.plugins), executorPlugin)
	cfg := runner.Config{
		AppName:        s.appName,
		Agent:          s.rootAgent,
		SessionService: s.sessions,
		MemoryService:  s.memories,
		PluginConfig:   runner.PluginConfig{Plugins: plugins},
	}
	built, err := runner.New(cfg)
	if err != nil {
		return adka2a.RunnerConfig{}, nil, fmt.Errorf("building the A2A runner: %w", err)
	}
	return adka2a.RunnerConfig{
			AppName:        cfg.AppName,
			Agent:          cfg.Agent,
			SessionService: cfg.SessionService,
		},
		&serializingRunner{inner: built, gate: s.gate},
		nil
}

// serializingRunner runs at most one ADK invocation per logical A2A session at
// a time.
//
// # Why the gate is here and not in a callback
//
// The session service hands back a detached snapshot of session state. Locking
// only the final append cannot stop two overlapping invocations from loading
// the same token totals and losing one update, because both loads happen before
// either append. The gate therefore has to be taken before ADK loads the
// session, which means around the whole run.
//
// This is the invocation-level sibling of the policy plane's per-session
// callback lock, not a replacement for it: that one serializes a
// read-modify-write inside one invocation's callbacks, this one serializes
// whole invocations. Different scopes, and neither subsumes the other.
//
// Different sessions still run concurrently, which is the point — a shared
// server-wide lock would serialize unrelated callers.
type serializingRunner struct {
	inner *runner.Runner
	gate  *sessionGate
}

// serializingRunner is the runner shape adka2a's executor accepts.
var _ adka2a.Runner = (*serializingRunner)(nil)

// Run implements adka2a.Runner.
//
// The lock is acquired inside the iterator rather than in Run itself, because
// an iter.Seq2 is a recipe: returning it does not start anything, and a lock
// taken by a sequence nobody ranges over would never be released. The deferred
// release covers every exit — the loop finishing, the consumer breaking early,
// and a panic in the agent.
func (r *serializingRunner) Run(
	ctx context.Context, userID, sessionID string, msg *genai.Content, cfg agent.RunConfig,
) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		release := r.gate.acquire(sessionKey{userID: userID, sessionID: sessionID})
		defer release()
		for event, err := range r.inner.Run(ctx, userID, sessionID, msg, cfg) {
			if !yield(event, err) {
				return
			}
		}
	}
}

// sessionKey is one logical A2A session: the caller and the context id, which
// adka2a maps onto ADK's user and session ids.
type sessionKey struct {
	userID    string
	sessionID string
}

// sessionGate hands out one lock per logical session, reference counted so an
// idle session leaves nothing behind.
//
// The reference count is what makes the map bounded: a server that kept a
// mutex per session id it had ever seen would grow without limit under a
// long-lived deployment, and the Python track asserts explicitly that both of
// its bookkeeping maps are empty once a turn finishes.
type sessionGate struct {
	locks map[sessionKey]*countedLock
	guard sync.Mutex
}

// countedLock is one session's lock plus the number of holders and waiters.
type countedLock struct {
	mutex sync.Mutex
	refs  int
}

// newSessionGate returns an empty gate.
func newSessionGate() *sessionGate {
	return &sessionGate{locks: make(map[sessionKey]*countedLock)}
}

// acquire takes the session's lock and returns the function that releases it.
func (g *sessionGate) acquire(key sessionKey) func() {
	g.guard.Lock()
	entry, ok := g.locks[key]
	if !ok {
		entry = &countedLock{}
		g.locks[key] = entry
	}
	// The reference is taken while the guard is held and before the wait, so a
	// waiter cannot have its lock deleted out from under it by a holder that
	// finishes first.
	entry.refs++
	g.guard.Unlock()

	entry.mutex.Lock()
	return func() {
		entry.mutex.Unlock()
		g.guard.Lock()
		defer g.guard.Unlock()
		if entry.refs--; entry.refs == 0 {
			delete(g.locks, key)
		}
	}
}

// tracked reports how many session locks the gate is holding. It exists for the
// test that proves an idle server leaves nothing behind.
func (g *sessionGate) tracked() int {
	g.guard.Lock()
	defer g.guard.Unlock()
	return len(g.locks)
}

// callBudget bounds how many model calls one invocation may make.
//
// # Why this is hand-built
//
// ADK Python bounds a run with RunConfig.max_llm_calls, which the Python A2A
// request converter set to AGENT_A2A_MAX_LLM_CALLS. ADK Go v2.3.0 has no such
// field: agent.RunConfig carries only StreamingMode and
// SaveInputBlobsAsArtifacts, and MaxLLMCalls exists solely on
// agent.LiveRunConfig, which is the bidirectional websocket path the A2A server
// never takes. Dropping the setting silently would remove the only bound on a
// model that keeps calling tools forever, so the bound is re-implemented at the
// seam ADK does offer — a plugin's before-model hook, which fires for every
// agent, sub-agent and workflow node in the application.
//
// The count is per invocation, not per session: one A2A turn is one invocation,
// and a long conversation is many bounded turns rather than one unbounded one.
type callBudget struct {
	counts map[string]int
	guard  sync.Mutex
	limit  int
}

// newCallBudgetPlugin builds the plugin carrying a per-invocation bound.
//
// A limit of zero or less means unbounded, and returns no plugin at all: an
// always-true guard on the hottest callback in the process is worth avoiding,
// and a nil plugin is what the caller then skips installing.
func newCallBudgetPlugin(limit int) (*plugin.Plugin, error) {
	if limit <= 0 {
		return nil, nil
	}
	budget := &callBudget{limit: limit, counts: make(map[string]int)}
	built, err := plugin.New(plugin.Config{
		Name:                CallBudgetPluginName,
		BeforeModelCallback: budget.beforeModel,
		AfterRunCallback:    budget.afterRun,
	})
	if err != nil {
		return nil, fmt.Errorf("building the %s plugin: %w", CallBudgetPluginName, err)
	}
	return built, nil
}

// beforeModel counts one model call and refuses the one past the bound.
//
// Returning an error rather than a substitute response is deliberate: a
// runaway plan is a failure, and the A2A task must end in TaskStateFailed so a
// caller can see it. A synthesized answer would look like a completed turn.
func (b *callBudget) beforeModel(ctx agent.Context, _ *model.LLMRequest) (*model.LLMResponse, error) {
	invocation := ctx.InvocationID()
	b.guard.Lock()
	b.counts[invocation]++
	used := b.counts[invocation]
	b.guard.Unlock()
	if used > b.limit {
		return nil, fmt.Errorf("%w: this turn asked the model for more than %d calls",
			ErrCallBudgetExceeded, b.limit)
	}
	return nil, nil
}

// afterRun drops the invocation's counter.
//
// The runner defers this callback inside the event iterator, so it runs when
// the run finishes, when the consumer stops reading early, and when the agent
// panics. That is what keeps the map bounded without a sweeper.
func (b *callBudget) afterRun(ctx agent.InvocationContext) {
	b.guard.Lock()
	defer b.guard.Unlock()
	delete(b.counts, ctx.InvocationID())
}

// tracked reports how many invocations the budget is still counting. It exists
// for the test that proves the counters are released.
func (b *callBudget) tracked() int {
	b.guard.Lock()
	defer b.guard.Unlock()
	return len(b.counts)
}

// recoveringSessions makes the executor's get-then-create safe under a
// first-use race.
//
// # The race
//
// adka2a's executor prepares the session outside the runner: it calls Get, and
// on any failure calls Create. Two first requests for the same context id can
// both observe the miss and both call Create; one of them then fails on the
// primary key and the whole execution fails, for a session that plainly exists.
//
// # The recovery, and its limits
//
// Only the executor's narrow contract is recovered — an explicit session id and
// no requested state — and only by reading back a session that genuinely
// exists. A create that failed for any other reason (a full disk, a missing
// directory) produces a Get that fails too, and the original Create error is
// what the caller sees.
//
// Matching on the driver's duplicate-key error would be more direct, but
// github.com/glebarez/sqlite implements no gorm error translator, so
// gorm.ErrDuplicatedKey is never produced and the alternative is matching on
// message text. Reading the row back is both driver-independent and stronger:
// it returns a session only when one is actually there.
type recoveringSessions struct {
	session.Service
}

// Create implements session.Service.
func (s recoveringSessions) Create(
	ctx context.Context, req *session.CreateRequest,
) (*session.CreateResponse, error) {
	created, err := s.Service.Create(ctx, req)
	if err == nil {
		return created, nil
	}
	if req == nil || req.SessionID == "" || len(req.State) > 0 {
		// Never discard requested state, and never invent an id: a general
		// create call must fail as it failed.
		return nil, err
	}
	existing, getErr := s.Get(ctx, &session.GetRequest{
		AppName:   req.AppName,
		UserID:    req.UserID,
		SessionID: req.SessionID,
	})
	if getErr != nil || existing == nil || existing.Session == nil {
		return nil, err
	}
	return &session.CreateResponse{Session: existing.Session}, nil
}
