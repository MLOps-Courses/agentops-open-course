package a2aserver_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/a2aserver"
)

// Concurrency and shutdown, over the real handler and — for the drain — a real
// listener. What matters here is that the guarantees hold for the process a
// deployment actually runs, not only for the primitives underneath.

// concurrencyProbe watches how many turns are inside the model at once.
//
// Each turn waits at a barrier for a fixed window. When turns are allowed to
// overlap they all arrive and the barrier releases them immediately; when they
// are serialized the first waits out its window alone, so the peak stays at
// one and the test fails on the count rather than on a lucky schedule.
type concurrencyProbe struct {
	arrived chan struct{}
	active  atomic.Int32
	peak    atomic.Int32
	window  time.Duration
	expect  int32
}

func newConcurrencyProbe(expect int32, window time.Duration) *concurrencyProbe {
	return &concurrencyProbe{arrived: make(chan struct{}), window: window, expect: expect}
}

// enter is the hold a scripted turn runs inside.
func (p *concurrencyProbe) enter(context.Context) {
	current := p.active.Add(1)
	for {
		highest := p.peak.Load()
		if current <= highest || p.peak.CompareAndSwap(highest, current) {
			break
		}
	}
	if current == p.expect {
		close(p.arrived)
	}
	select {
	case <-p.arrived:
	case <-time.After(p.window):
	}
	p.active.Add(-1)
}

// holds builds the per-turn hold table the scripted model takes.
func (p *concurrencyProbe) holds(turns int) map[int]func(ctx context.Context) {
	table := make(map[int]func(ctx context.Context), turns)
	for index := range turns {
		table[index] = p.enter
	}
	return table
}

// TestOneSessionRunsOneTurnAtATime is the guarantee the Python track needed a
// runner subclass for.
//
// The session service hands back a detached snapshot, so two overlapping turns
// on one session would both read the same token totals and one update would
// vanish. Serializing whole invocations — not just the final append — is the
// only thing that closes it.
func TestOneSessionRunsOneTurnAtATime(t *testing.T) {
	t.Parallel()

	const turns = 3
	probe := newConcurrencyProbe(turns, 300*time.Millisecond)
	model := &scriptedLLM{
		turns: [][]*adkmodel.LLMResponse{textTurn("One."), textTurn("Two."), textTurn("Three.")},
		hold:  probe.holds(turns),
	}
	fixture := newFixture(t, func(opts *fixtureOptions) { opts.model = model })
	server := fixture.serve(t)

	// One shared context id: the first turn establishes it, and the rest join
	// the same logical session.
	first := streamResults(t, server, textMessage("m0", "First."))
	contextID, _ := first[0]["contextId"].(string)
	if contextID == "" {
		t.Fatal("the first turn produced no context id")
	}
	probe.peak.Store(0)

	var wait sync.WaitGroup
	for index := range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			streamResults(t, server, map[string]any{"message": map[string]any{
				"kind":      "message",
				"messageId": "m" + string(rune('1'+index)),
				"role":      "user",
				"contextId": contextID,
				"parts":     []any{map[string]any{"kind": "text", "text": "Concurrent."}},
			}})
		}()
	}
	wait.Wait()

	if got := probe.peak.Load(); got != 1 {
		t.Errorf("peak concurrent turns in one session = %d, want 1", got)
	}
}

// TestDifferentSessionsRunConcurrently is the other half: the gate must be per
// session, because a server-wide lock would serialize unrelated callers and
// turn a multi-tenant deployment into a queue.
func TestDifferentSessionsRunConcurrently(t *testing.T) {
	t.Parallel()

	const turns = 2
	probe := newConcurrencyProbe(turns, 5*time.Second)
	model := &scriptedLLM{
		turns: [][]*adkmodel.LLMResponse{textTurn("One."), textTurn("Two.")},
		hold:  probe.holds(turns),
	}
	fixture := newFixture(t, func(opts *fixtureOptions) { opts.model = model })
	server := fixture.serve(t)

	var wait sync.WaitGroup
	for index := range turns {
		wait.Add(1)
		go func() {
			defer wait.Done()
			// No contextId: each request starts its own session.
			streamResults(t, server,
				textMessage("m"+string(rune('0'+index)), "Independent."))
		}()
	}
	wait.Wait()

	if got := probe.peak.Load(); got != turns {
		t.Errorf("peak concurrent turns across sessions = %d, want %d", got, turns)
	}
}

// TestConcurrentFirstUseOfOneSessionSucceeds covers the race the Python session
// subclass existed for: the A2A executor does get-then-create outside the
// runner, so two first requests for one context id can both observe a miss and
// both create.
func TestConcurrentFirstUseOfOneSessionSucceeds(t *testing.T) {
	t.Parallel()

	const callers = 6
	turns := make([][]*adkmodel.LLMResponse, 0, callers)
	for range callers {
		turns = append(turns, textTurn("Answered."))
	}
	fixture := newFixture(t, func(opts *fixtureOptions) {
		opts.model = &scriptedLLM{turns: turns}
	})
	server := fixture.serve(t)

	contextID := "shared-first-use"
	var (
		wait     sync.WaitGroup
		guard    sync.Mutex
		terminal []string
	)
	for index := range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results := streamResults(t, server, map[string]any{"message": map[string]any{
				"kind":      "message",
				"messageId": "m" + string(rune('a'+index)),
				"role":      "user",
				"contextId": contextID,
				"parts":     []any{map[string]any{"kind": "text", "text": "Race."}},
			}})
			states := states(results)
			guard.Lock()
			defer guard.Unlock()
			terminal = append(terminal, states[len(states)-1])
		}()
	}
	wait.Wait()

	guard.Lock()
	defer guard.Unlock()
	for index, state := range terminal {
		if state != "completed" {
			t.Errorf("caller %d ended %q, want %q", index, state, "completed")
		}
	}
	// One session, not six: the recovery reads the existing row back rather
	// than inventing a second one.
	if got := sessionRowCount(t, fixture, contextID); got != 1 {
		t.Errorf("%d session rows for one context id, want 1", got)
	}
}

// TestServeDrainsOnCancellation runs the real listener, because the drain is a
// property of the http.Server this package builds rather than of its handler.
func TestServeDrainsOnCancellation(t *testing.T) {
	t.Parallel()

	port := freePort(t)
	fixture := newUnstartedFixture(t, func(opts *fixtureOptions) {
		opts.options = func(options *a2aserver.Options) {
			options.Port = port
			options.DrainTimeout = 2 * time.Second
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- fixture.server.Serve(ctx) }()

	address := "http://127.0.0.1:" + strconv.Itoa(port)
	waitForLiveness(t, address)

	cancel()
	select {
	case err := <-served:
		// A canceled context is a normal shutdown, not a failure: Ctrl-C and
		// SIGTERM both arrive here.
		if err != nil {
			t.Errorf("Serve() error = %v, want nil after cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve() did not return after cancellation")
	}

	// The listener is closed, so the port no longer answers.
	request, err := http.NewRequestWithContext(context.WithoutCancel(ctx), http.MethodGet,
		address+a2aserver.LivenessPath, nil)
	if err != nil {
		t.Fatalf("building the post-shutdown request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err == nil {
		_ = response.Body.Close()
		t.Error("the server still answers after shutdown")
	}
}

// TestServeReportsAStartupFailure keeps the ordering honest at the process
// boundary: a refused startup must not leave a listener behind.
func TestServeReportsAStartupFailure(t *testing.T) {
	t.Parallel()

	fixture := newUnstartedFixture(t)
	fixture.steps.fail("recover", errors.New("state directory is unreadable"))

	err := fixture.server.Serve(t.Context())
	if err == nil {
		t.Fatal("Serve() error = nil, want the startup failure")
	}
	if fixture.server.TaskStore() != nil {
		t.Error("a refused startup opened the task store")
	}
	if got := fixture.steps.sequence(); len(got) != 1 || got[0] != "recover" {
		t.Errorf("startup ran %v, want it to stop at the first failure", got)
	}
}

// freePort reserves a port the operating system just handed out.
func freePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address = %T, want *net.TCPAddr", listener.Addr())
	}
	port := address.Port
	if err := listener.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return port
}

// waitForLiveness blocks until the server answers its liveness probe.
func waitForLiveness(t *testing.T, address string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
			address+a2aserver.LivenessPath, nil)
		if err != nil {
			t.Fatalf("building the liveness request: %v", err)
		}
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the server never became live")
}
