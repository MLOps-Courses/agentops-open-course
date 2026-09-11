package a2aserver

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

// The three concurrency mechanisms this package owns, tested where their
// bookkeeping is visible. Their end-to-end behavior is asserted over the wire
// in protocol_test.go; what is asserted here is that neither the lock table nor
// the call counters grow without bound, which no wire test can observe.

func TestSessionGateSerializesOneSession(t *testing.T) {
	t.Parallel()

	gate := newSessionGate()
	key := sessionKey{userID: "u", sessionID: "s"}

	var (
		active atomic.Int32
		peak   atomic.Int32
		wait   sync.WaitGroup
	)
	for range 4 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			release := gate.acquire(key)
			defer release()
			current := active.Add(1)
			for {
				highest := peak.Load()
				if current <= highest || peak.CompareAndSwap(highest, current) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			active.Add(-1)
		}()
	}
	wait.Wait()

	if got := peak.Load(); got != 1 {
		t.Errorf("peak concurrency = %d, want 1", got)
	}
	if got := gate.tracked(); got != 0 {
		t.Errorf("%d session locks survived, want 0", got)
	}
}

func TestSessionGateKeepsDifferentSessionsConcurrent(t *testing.T) {
	t.Parallel()

	gate := newSessionGate()
	// A barrier rather than a sleep: if the gate serialized unrelated sessions,
	// the second holder would never reach the barrier and the test would fail
	// on the timeout rather than on a lucky schedule.
	const holders = 3
	var (
		arrived  sync.WaitGroup
		released = make(chan struct{})
		wait     sync.WaitGroup
	)
	arrived.Add(holders)
	for index := range holders {
		wait.Add(1)
		go func() {
			defer wait.Done()
			release := gate.acquire(sessionKey{userID: "u", sessionID: string(rune('a' + index))})
			defer release()
			arrived.Done()
			<-released
		}()
	}

	done := make(chan struct{})
	go func() { arrived.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("different sessions did not run concurrently")
	}
	close(released)
	wait.Wait()

	if got := gate.tracked(); got != 0 {
		t.Errorf("%d session locks survived, want 0", got)
	}
}

func TestCallBudgetCountsPerInvocationAndReleases(t *testing.T) {
	t.Parallel()

	budget := &callBudget{limit: 2, counts: map[string]int{}}
	first := &budgetContext{invocation: "inv-1"}
	second := &budgetContext{invocation: "inv-2"}

	for _, ctx := range []*budgetContext{first, second} {
		for attempt := range 2 {
			if _, err := budget.beforeModel(ctx, nil); err != nil {
				t.Fatalf("call %d of %s error = %v, want nil", attempt+1, ctx.invocation, err)
			}
		}
	}
	// Two invocations, each at its own limit: the bound is per turn, so one
	// caller's long turn cannot spend another caller's budget.
	if got := budget.tracked(); got != 2 {
		t.Errorf("%d invocations tracked, want 2", got)
	}

	_, err := budget.beforeModel(first, nil)
	if !errors.Is(err, ErrCallBudgetExceeded) {
		t.Errorf("the third call error = %v, want %v", err, ErrCallBudgetExceeded)
	}
	if _, err := budget.beforeModel(second, nil); !errors.Is(err, ErrCallBudgetExceeded) {
		t.Errorf("the second invocation was not bounded: %v", err)
	}

	budget.afterRun(first)
	budget.afterRun(second)
	if got := budget.tracked(); got != 0 {
		t.Errorf("%d invocation counters survived, want 0", got)
	}
}

func TestAnUnboundedBudgetBuildsNoPlugin(t *testing.T) {
	t.Parallel()

	for _, limit := range []int{0, -1} {
		built, err := newCallBudgetPlugin(limit)
		if err != nil {
			t.Fatalf("newCallBudgetPlugin(%d) error = %v, want nil", limit, err)
		}
		if built != nil {
			t.Errorf("newCallBudgetPlugin(%d) = %v, want nil", limit, built)
		}
	}
}

func TestRecoveringSessionsReadsBackAConcurrentCreate(t *testing.T) {
	t.Parallel()

	existing := &fakeSession{id: "s1"}
	service := &fakeSessions{
		createErr: errors.New("UNIQUE constraint failed: sessions.id"),
		getResult: existing,
	}
	recovering := recoveringSessions{Service: service}

	created, err := recovering.Create(t.Context(), &session.CreateRequest{
		AppName: "app", UserID: "u", SessionID: "s1", State: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Create() error = %v, want the existing session", err)
	}
	if created.Session != existing {
		t.Errorf("Create() = %v, want the session that already existed", created.Session)
	}
}

func TestRecoveringSessionsNeverDiscardsRequestedState(t *testing.T) {
	t.Parallel()

	failure := errors.New("UNIQUE constraint failed: sessions.id")
	service := &fakeSessions{createErr: failure, getResult: &fakeSession{id: "s1"}}
	recovering := recoveringSessions{Service: service}

	for _, testCase := range []struct {
		request *session.CreateRequest
		name    string
	}{
		{
			// Returning the existing session here would silently drop the state
			// the caller asked to seed it with.
			name:    "requested state",
			request: &session.CreateRequest{AppName: "app", UserID: "u", SessionID: "s1", State: map[string]any{"k": "v"}},
		},
		{
			// Without an explicit id the service would have generated one, so a
			// duplicate is not the failure being recovered from.
			name:    "generated id",
			request: &session.CreateRequest{AppName: "app", UserID: "u"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if _, err := recovering.Create(t.Context(), testCase.request); !errors.Is(err, failure) {
				t.Errorf("Create() error = %v, want the original failure", err)
			}
		})
	}
}

func TestRecoveringSessionsReportsARealFailure(t *testing.T) {
	t.Parallel()

	failure := errors.New("disk full")
	// A create that failed for any other reason produces a get that fails too,
	// and the caller has to see the original cause rather than "not found".
	service := &fakeSessions{createErr: failure, getErr: errors.New("no such table")}
	recovering := recoveringSessions{Service: service}

	_, err := recovering.Create(t.Context(), &session.CreateRequest{
		AppName: "app", UserID: "u", SessionID: "s1", State: map[string]any{},
	})
	if !errors.Is(err, failure) {
		t.Errorf("Create() error = %v, want the original failure", err)
	}
}

// budgetContext is the smallest agent.Context the call budget reads: it needs
// an invocation id and nothing else.
type budgetContext struct {
	agent.StrictContextMock
	invocation string
}

func (c *budgetContext) InvocationID() string { return c.invocation }

// fakeSessions is a session.Service whose outcomes a test dictates.
type fakeSessions struct {
	createErr error
	getErr    error
	getResult session.Session
}

func (f *fakeSessions) Create(context.Context, *session.CreateRequest) (*session.CreateResponse, error) {
	return nil, f.createErr
}

func (f *fakeSessions) Get(context.Context, *session.GetRequest) (*session.GetResponse, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &session.GetResponse{Session: f.getResult}, nil
}

func (f *fakeSessions) List(context.Context, *session.ListRequest) (*session.ListResponse, error) {
	return &session.ListResponse{}, nil
}

func (f *fakeSessions) Delete(context.Context, *session.DeleteRequest) error { return nil }

func (f *fakeSessions) AppendEvent(context.Context, session.Session, *session.Event) error {
	return nil
}

// fakeSession is the smallest session.Session a recovery can return.
type fakeSession struct{ id string }

func (f *fakeSession) ID() string                { return f.id }
func (f *fakeSession) AppName() string           { return "app" }
func (f *fakeSession) UserID() string            { return "u" }
func (f *fakeSession) State() session.State      { return nil }
func (f *fakeSession) Events() session.Events    { return nil }
func (f *fakeSession) LastUpdateTime() time.Time { return time.Time{} }
