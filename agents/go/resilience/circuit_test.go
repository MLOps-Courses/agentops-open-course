package resilience

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// This file is the Go port of tests/test_circuit.py.
//
// The Python suite injected a fake monotonic clock so no transition needed real
// time to pass. Go does it one level down: every case that depends on the
// cooldown runs inside a testing/synctest bubble, where time.Now itself is
// virtual. That covers the retry timers in Guard too, which an injected breaker
// clock could never have reached.

// newTestBreaker builds one breaker and fails the test rather than returning an
// error, because a breaker that will not build is a bug in the case.
func newTestBreaker(t *testing.T, cfg BreakerConfig) *Breaker {
	t.Helper()

	breaker, err := NewBreaker(cfg)
	if err != nil {
		t.Fatalf("NewBreaker() error = %v, want nil", err)
	}
	return breaker
}

// mustAllow admits one call, failing the test when the breaker refuses.
func mustAllow(t *testing.T, breaker *Breaker) Permit {
	t.Helper()

	permit, ok := breaker.Allow()
	if !ok {
		t.Fatalf("Allow() refused a call while the breaker was %s", breaker.State())
	}
	return permit
}

// failOnce records one admitted failure, the way the Python `_fail` helper did.
func failOnce(t *testing.T, breaker *Breaker) {
	t.Helper()

	breaker.RecordFailure(t.Context(), mustAllow(t, breaker))
}

// openingRecorder captures every opening the breaker announces, which is the
// observation point the OTel counter is wired to in production.
type openingRecorder struct {
	resources []string
	mu        sync.Mutex
}

func (r *openingRecorder) observe(_ context.Context, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.resources = append(r.resources, name)
}

func (r *openingRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.resources...)
}

// TestABreakerOpensAfterConsecutiveFailures is the port of
// test_opens_after_consecutive_failures.
func TestABreakerOpensAfterConsecutiveFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		breaker := newTestBreaker(t, BreakerConfig{
			Name: readTool, FailureThreshold: 3, ResetTimeout: 30 * time.Second,
		})

		for range 2 {
			failOnce(t, breaker)
		}
		if got := breaker.State(); got != StateClosed {
			t.Errorf("State() = %s after 2 of 3 failures, want %s", got, StateClosed)
		}

		failOnce(t, breaker)
		if got := breaker.State(); got != StateOpen {
			t.Errorf("State() = %s after the threshold failure, want %s", got, StateOpen)
		}
		if _, ok := breaker.Allow(); ok {
			t.Error("Allow() admitted a call while the breaker was open")
		}
	})
}

// TestACooldownAdmitsOneProbeAndASuccessClosesIt is the port of
// test_half_open_after_cooldown_then_closes_on_success, including the boundary
// the Python suite pinned at one second either side of the cooldown.
func TestACooldownAdmitsOneProbeAndASuccessClosesIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		breaker := newTestBreaker(t, BreakerConfig{
			Name: readTool, FailureThreshold: 1, ResetTimeout: 30 * time.Second,
		})

		failOnce(t, breaker)
		if _, ok := breaker.Allow(); ok {
			t.Fatal("Allow() admitted a call immediately after the breaker opened")
		}

		time.Sleep(29 * time.Second)
		if _, ok := breaker.Allow(); ok {
			t.Error("Allow() admitted a probe one second before the cooldown elapsed")
		}

		// The comparison is strictly less than, so the probe is admitted at exactly
		// the cooldown rather than one tick after it.
		time.Sleep(time.Second)
		permit := mustAllow(t, breaker)
		if got := breaker.State(); got != StateHalfOpen {
			t.Errorf("State() = %s once the probe was admitted, want %s", got, StateHalfOpen)
		}

		breaker.RecordSuccess(permit)
		if got := breaker.State(); got != StateClosed {
			t.Errorf("State() = %s after a successful probe, want %s", got, StateClosed)
		}
		if got := breaker.snapshot().failures; got != 0 {
			t.Errorf("failures = %d after recovery, want 0", got)
		}
	})
}

// TestHalfOpenAdmitsExactlyOneConcurrentProbe is the port of
// test_half_open_allows_only_one_concurrent_probe. Admitting two would defeat
// the point: recovery is tested with one call, not with the load that broke it.
func TestHalfOpenAdmitsExactlyOneConcurrentProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		breaker := newTestBreaker(t, BreakerConfig{
			Name: readTool, FailureThreshold: 1, ResetTimeout: 30 * time.Second,
		})
		failOnce(t, breaker)
		time.Sleep(30 * time.Second)

		const contenders = 8
		start := make(chan struct{})
		admitted := make(chan bool, contenders)
		var group sync.WaitGroup
		group.Add(contenders)
		for range contenders {
			go func() {
				defer group.Done()

				<-start
				_, ok := breaker.Allow()
				admitted <- ok
			}()
		}
		close(start)
		group.Wait()
		close(admitted)

		granted := 0
		for ok := range admitted {
			if ok {
				granted++
			}
		}
		if granted != 1 {
			t.Errorf("%d of %d contenders were admitted, want exactly 1", granted, contenders)
		}
		if got := breaker.State(); got != StateHalfOpen {
			t.Errorf("State() = %s, want %s", got, StateHalfOpen)
		}
	})
}

// TestAFailureWhileOpenChangesNothing is the port of
// test_repeated_failure_while_open_keeps_one_opened_transition. A stale outcome
// arriving during the cooldown must not extend it.
func TestAFailureWhileOpenChangesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		breaker := newTestBreaker(t, BreakerConfig{
			Name: readTool, FailureThreshold: 1, ResetTimeout: 30 * time.Second,
		})

		stale := mustAllow(t, breaker)
		breaker.RecordFailure(context.Background(), stale)
		before := breaker.snapshot()
		if before.state != StateOpen {
			t.Fatalf("State() = %s, want %s", before.state, StateOpen)
		}

		time.Sleep(5 * time.Second)
		breaker.RecordFailure(context.Background(), stale)

		after := breaker.snapshot()
		if after.state != StateOpen {
			t.Errorf("State() = %s, want %s", after.state, StateOpen)
		}
		if after.failures != 1 {
			t.Errorf("failures = %d, want the count left at 1", after.failures)
		}
		if !after.openedAt.Equal(before.openedAt) {
			t.Errorf("openedAt moved to %s, want the running cooldown untouched", after.openedAt)
		}
	})
}

// TestAHalfOpenFailureReopensWithAFreshCooldown is the port of
// test_half_open_failure_reopens_with_fresh_cooldown. The trial call proved the
// dependency is still unhealthy, so the wait restarts from now — whatever the
// failure count says.
func TestAHalfOpenFailureReopensWithAFreshCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		breaker := newTestBreaker(t, BreakerConfig{
			Name: readTool, FailureThreshold: 1, ResetTimeout: 10 * time.Second,
		})
		start := time.Now()

		failOnce(t, breaker)
		time.Sleep(10 * time.Second)
		permit := mustAllow(t, breaker)

		time.Sleep(2 * time.Second)
		breaker.RecordFailure(context.Background(), permit)

		snapshot := breaker.snapshot()
		if snapshot.state != StateOpen {
			t.Errorf("State() = %s after a failed probe, want %s", snapshot.state, StateOpen)
		}
		if elapsed := snapshot.openedAt.Sub(start); elapsed != 12*time.Second {
			t.Errorf("openedAt is %s into the test, want the cooldown restarted at 12s", elapsed)
		}
		if _, ok := breaker.Allow(); ok {
			t.Error("Allow() admitted a call during the fresh cooldown")
		}
	})
}

// TestACanceledProbeReopensWithoutCountingAFailure is the port of the Python
// suite's `..._half_open_probe_reopens_without_counting_a_failure` case (spelled
// there for an aborted asyncio task). It is driven through the guard, as the
// Python case was, because the interesting part is that the wrapper releases the
// probe when its caller stops waiting.
func TestACanceledProbeReopensWithoutCountingAFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		breakers, err := NewBreakers(BreakersConfig{FailureThreshold: 1, ResetTimeout: 30 * time.Second})
		if err != nil {
			t.Fatalf("NewBreakers() error = %v, want nil", err)
		}
		guard := newTestGuard(t, Config{Breakers: breakers, ToolTimeout: time.Hour, MaxRetries: 0})
		start := time.Now()

		if err := guard.Run(context.Background(), probeTool, func(context.Context) error {
			return errTransient
		}); err == nil {
			t.Fatal("Run() error = nil, want the single attempt to fail")
		}
		breaker := breakers.Get(probeTool)
		if got := breaker.State(); got != StateOpen {
			t.Fatalf("State() = %s after the threshold failure, want %s", got, StateOpen)
		}
		failuresBefore := breaker.snapshot().failures

		time.Sleep(30 * time.Second)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- guard.Run(ctx, probeTool, func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			})
		}()

		// The probe is now admitted and parked inside the tool, holding the one
		// half-open slot.
		synctest.Wait()
		if got := breaker.State(); got != StateHalfOpen {
			t.Fatalf("State() = %s while the probe is in flight, want %s", got, StateHalfOpen)
		}

		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want it to carry context.Canceled", err)
		}

		snapshot := breaker.snapshot()
		if snapshot.state != StateOpen {
			t.Errorf("State() = %s after the probe was abandoned, want %s", snapshot.state, StateOpen)
		}
		if snapshot.failures != failuresBefore {
			t.Errorf("failures = %d, want %d — a caller that gave up proved nothing",
				snapshot.failures, failuresBefore)
		}
		if _, ok := breaker.Allow(); ok {
			t.Error("Allow() admitted a call during the cooldown the abandonment restarted")
		}
		if elapsed := time.Since(start); elapsed != 30*time.Second {
			t.Errorf("the case took %s of virtual time, want only the 30s cooldown", elapsed)
		}
	})
}

// TestAbandoningAClosedOrRetiredPermitIsANoop is the port of
// test_abandoning_a_closed_or_stale_permit_is_a_noop. Abandonment only releases
// a probe that is actually holding the half-open slot.
func TestAbandoningAClosedOrRetiredPermitIsANoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		breaker := newTestBreaker(t, BreakerConfig{
			Name: readTool, FailureThreshold: 1, ResetTimeout: 10 * time.Second,
		})

		closedPermit := mustAllow(t, breaker)
		breaker.RecordAbandoned(context.Background(), closedPermit)
		if got := breaker.State(); got != StateClosed {
			t.Errorf("State() = %s after abandoning a closed-state call, want %s", got, StateClosed)
		}

		breaker.RecordFailure(context.Background(), closedPermit)
		time.Sleep(10 * time.Second)
		probePermit := mustAllow(t, breaker)
		breaker.RecordSuccess(probePermit)
		breaker.RecordAbandoned(context.Background(), probePermit)
		if got := breaker.State(); got != StateClosed {
			t.Errorf("State() = %s after abandoning an already-settled probe, want %s", got, StateClosed)
		}
	})
}

// TestAStaleSuccessCannotCloseABreakerANewerFailureOpened is the port of
// test_stale_success_cannot_close_a_breaker_opened_by_a_newer_outcome.
func TestAStaleSuccessCannotCloseABreakerANewerFailureOpened(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		breaker := newTestBreaker(t, BreakerConfig{
			Name: readTool, FailureThreshold: 1, ResetTimeout: 30 * time.Second,
		})

		staleSuccess := mustAllow(t, breaker)
		newerFailure := mustAllow(t, breaker)

		breaker.RecordFailure(context.Background(), newerFailure)
		breaker.RecordSuccess(staleSuccess)

		snapshot := breaker.snapshot()
		if snapshot.state != StateOpen {
			t.Errorf("State() = %s, want the stale success ignored and the breaker %s",
				snapshot.state, StateOpen)
		}
		if snapshot.failures != 1 {
			t.Errorf("failures = %d, want 1", snapshot.failures)
		}
	})
}

// TestAStaleFailureCannotReopenARecoveredGeneration is the port of
// test_stale_failure_cannot_reopen_a_recovered_breaker_generation. This is the
// case the generation counter exists for: a straggler from the failed era must
// not be able to undo a recovery it knows nothing about.
func TestAStaleFailureCannotReopenARecoveredGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		breaker := newTestBreaker(t, BreakerConfig{
			Name: readTool, FailureThreshold: 1, ResetTimeout: 10 * time.Second,
		})

		staleFailure := mustAllow(t, breaker)
		newerFailure := mustAllow(t, breaker)
		breaker.RecordFailure(context.Background(), newerFailure)

		time.Sleep(10 * time.Second)
		recoveryProbe := mustAllow(t, breaker)
		breaker.RecordSuccess(recoveryProbe)

		breaker.RecordFailure(context.Background(), staleFailure)

		snapshot := breaker.snapshot()
		if snapshot.state != StateClosed {
			t.Errorf("State() = %s, want the recovered breaker to stay %s", snapshot.state, StateClosed)
		}
		if snapshot.failures != 0 {
			t.Errorf("failures = %d, want 0", snapshot.failures)
		}
	})
}

// TestConcurrentFailuresAreNotLost is the port of
// test_concurrent_failures_are_not_lost. Python had to slow the increment down
// to widen the window; Go runs this under -race, which reports the unsynchronized
// read-modify-write directly rather than waiting for it to lose.
func TestConcurrentFailuresAreNotLost(t *testing.T) {
	t.Parallel()

	const contenders = 16
	breaker := newTestBreaker(t, BreakerConfig{
		Name: readTool, FailureThreshold: 100, ResetTimeout: 30 * time.Second,
	})

	start := make(chan struct{})
	var group sync.WaitGroup
	group.Add(contenders)
	for range contenders {
		go func() {
			defer group.Done()

			<-start
			permit, ok := breaker.Allow()
			if !ok {
				return
			}
			breaker.RecordFailure(context.Background(), permit)
		}()
	}
	close(start)
	group.Wait()

	snapshot := breaker.snapshot()
	if snapshot.failures != contenders {
		t.Errorf("failures = %d, want %d — an increment was lost", snapshot.failures, contenders)
	}
	if snapshot.state != StateClosed {
		t.Errorf("State() = %s below the threshold, want %s", snapshot.state, StateClosed)
	}
}

// TestConcurrentRegistryAccessCreatesOneBreaker is the port of
// test_concurrent_registry_access_creates_one_breaker. Python counted
// constructor calls; the property that actually matters is that every caller
// ends up sharing one breaker, because two would each carry half the evidence
// and neither would ever trip.
func TestConcurrentRegistryAccessCreatesOneBreaker(t *testing.T) {
	t.Parallel()

	breakers, err := NewBreakers(BreakersConfig{FailureThreshold: 5, ResetTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("NewBreakers() error = %v, want nil", err)
	}

	const contenders = 16
	start := make(chan struct{})
	resolved := make(chan *Breaker, contenders)
	var group sync.WaitGroup
	group.Add(contenders)
	for range contenders {
		go func() {
			defer group.Done()

			<-start
			resolved <- breakers.Get(readTool)
		}()
	}
	close(start)
	group.Wait()
	close(resolved)

	var first *Breaker
	for breaker := range resolved {
		if first == nil {
			first = breaker
			continue
		}
		if breaker != first {
			t.Fatal("Get() handed out two different breakers for one resource")
		}
	}
	if names := breakers.Names(); len(names) != 1 || names[0] != readTool {
		t.Errorf("Names() = %v, want exactly [%s]", names, readTool)
	}
}

// TestTheGuardFailsFastWhileTheCircuitIsOpen is the port of
// test_with_resilience_fails_fast_when_circuit_open. The open circuit must shed
// the call entirely, not merely fail it after paying the retry budget.
func TestTheGuardFailsFastWhileTheCircuitIsOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		breakers, err := NewBreakers(BreakersConfig{FailureThreshold: 1, ResetTimeout: 30 * time.Second})
		if err != nil {
			t.Fatalf("NewBreakers() error = %v, want nil", err)
		}
		guard := newTestGuard(t, Config{Breakers: breakers, ToolTimeout: 30 * time.Second, MaxRetries: 0})

		calls := 0
		failing := func(context.Context) error {
			calls++
			return errTransient
		}

		var exhausted *RetriesExhaustedError
		if err := guard.Run(context.Background(), readTool, failing); !errors.As(err, &exhausted) {
			t.Fatalf("Run() error = %v, want a *RetriesExhaustedError", err)
		}
		if exhausted.Attempts != 1 {
			t.Errorf("Attempts = %d, want 1", exhausted.Attempts)
		}
		if got := breakers.Get(readTool).State(); got != StateOpen {
			t.Fatalf("State() = %s after the threshold failure, want %s", got, StateOpen)
		}

		var open *CircuitOpenError
		if err := guard.Run(context.Background(), readTool, failing); !errors.As(err, &open) {
			t.Fatalf("Run() error = %v, want a *CircuitOpenError", err)
		}
		if open.ResetTimeout != 30*time.Second {
			t.Errorf("ResetTimeout = %s, want the configured 30s", open.ResetTimeout)
		}
		if !strings.Contains(open.Error(), "AGENT_CIRCUIT_RESET_TIMEOUT_S") {
			t.Errorf("CircuitOpenError = %q, want it to name the setting that governs the wait", open)
		}
		if calls != 1 {
			t.Errorf("the tool ran %d times, want 1 — the open circuit shed the second call", calls)
		}
	})
}

// TestASuccessKeepsTheCircuitClosed is the port of
// test_success_keeps_circuit_closed.
func TestASuccessKeepsTheCircuitClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		breakers, err := NewBreakers(BreakersConfig{FailureThreshold: 2, ResetTimeout: 30 * time.Second})
		if err != nil {
			t.Fatalf("NewBreakers() error = %v, want nil", err)
		}
		guard := newTestGuard(t, Config{Breakers: breakers, ToolTimeout: 30 * time.Second, MaxRetries: 0})

		if err := guard.Run(context.Background(), readTool, func(context.Context) error {
			return nil
		}); err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		if got := breakers.Get(readTool).State(); got != StateClosed {
			t.Errorf("State() = %s after a healthy call, want %s", got, StateClosed)
		}
	})
}

// TestADeadlineCountsAsABreakerFailure is the port of
// test_deadline_counts_as_a_breaker_failure: a dependency that reliably times
// out is exactly the kind the breaker exists to stop calling.
func TestADeadlineCountsAsABreakerFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		breakers, err := NewBreakers(BreakersConfig{FailureThreshold: 1, ResetTimeout: 30 * time.Second})
		if err != nil {
			t.Fatalf("NewBreakers() error = %v, want nil", err)
		}
		guard := newTestGuard(t, Config{
			Breakers: breakers, ToolTimeout: 5 * time.Second, MaxRetries: 3, RetryBackoff: time.Second,
		})

		var deadline *DeadlineError
		if err := guard.Run(context.Background(), readTool, func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}); !errors.As(err, &deadline) {
			t.Fatalf("Run() error = %v, want a *DeadlineError", err)
		}
		if got := breakers.Get(readTool).State(); got != StateOpen {
			t.Errorf("State() = %s after a persistent timeout, want %s", got, StateOpen)
		}
	})
}

// TestOneSequenceOfFailedAttemptsCountsAsOneBreakerFailure pins the accounting
// the Python source called out in a comment and no single Python test isolated:
// a three-attempt exhaustion is one failing call, not three. Counting attempts
// would make the breaker trip MaxRetries+1 times faster than its configured
// threshold says.
func TestOneSequenceOfFailedAttemptsCountsAsOneBreakerFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		breakers, err := NewBreakers(BreakersConfig{FailureThreshold: 2, ResetTimeout: 30 * time.Second})
		if err != nil {
			t.Fatalf("NewBreakers() error = %v, want nil", err)
		}
		guard := newTestGuard(t, Config{
			Breakers: breakers, ToolTimeout: 30 * time.Second, MaxRetries: 2, RetryBackoff: 500 * time.Millisecond,
		})

		failing := func(context.Context) error { return errTransient }
		if err := guard.Run(context.Background(), readTool, failing); err == nil {
			t.Fatal("Run() error = nil, want the retries to be exhausted")
		}

		breaker := breakers.Get(readTool)
		snapshot := breaker.snapshot()
		if snapshot.failures != 1 {
			t.Errorf("failures = %d after 3 failed attempts, want 1", snapshot.failures)
		}
		if snapshot.state != StateClosed {
			t.Errorf("State() = %s below the threshold of 2, want %s", snapshot.state, StateClosed)
		}

		if err := guard.Run(context.Background(), readTool, failing); err == nil {
			t.Fatal("Run() error = nil, want the retries to be exhausted")
		}
		if got := breaker.State(); got != StateOpen {
			t.Errorf("State() = %s after the second failing call, want %s", got, StateOpen)
		}
	})
}

// TestADisabledCircuitLeavesRetryOnlyBehaviour is the port of
// test_disabled_circuit_leaves_retry_only_behavior. Python asserted the registry
// stayed empty; in Go a disabled breaker has no registry at all, which is the
// stronger form of the same statement.
func TestADisabledCircuitLeavesRetryOnlyBehaviour(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		guard := newTestGuard(t, Config{ToolTimeout: 30 * time.Second, MaxRetries: 0})
		if guard.Breakers() != nil {
			t.Error("Breakers() is non-nil while circuit breaking is off")
		}

		calls := 0
		for range 3 {
			var exhausted *RetriesExhaustedError
			if err := guard.Run(context.Background(), readTool, func(context.Context) error {
				calls++
				return errTransient
			}); !errors.As(err, &exhausted) {
				t.Fatalf("Run() error = %v, want a *RetriesExhaustedError", err)
			}
		}
		if calls != 3 {
			t.Errorf("the tool ran %d times, want 3 — with no breaker every call reaches it", calls)
		}
	})
}

// TestEveryOpeningIsAnnouncedOnce covers the observation point the OTel counter
// is wired to. The counter itself measures an opening *rate*, so a transition
// that opened the breaker without announcing it, or announced it twice, would
// make the Chapter 7.2 alert lie in either direction.
func TestEveryOpeningIsAnnouncedOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		recorder := &openingRecorder{}
		breaker := newTestBreaker(t, BreakerConfig{
			OnOpen: recorder.observe, Name: readTool, FailureThreshold: 1, ResetTimeout: 10 * time.Second,
		})

		// Open, then fail the recovery probe, then abandon the next one: the three
		// distinct edges into the open state.
		failOnce(t, breaker)
		time.Sleep(10 * time.Second)
		breaker.RecordFailure(context.Background(), mustAllow(t, breaker))
		time.Sleep(10 * time.Second)
		breaker.RecordAbandoned(context.Background(), mustAllow(t, breaker))

		// A stale outcome and a failure while open must announce nothing.
		breaker.RecordFailure(context.Background(), Permit{})

		want := []string{readTool, readTool, readTool}
		if got := recorder.seen(); len(got) != len(want) {
			t.Errorf("the breaker announced %v, want one entry per opening: %v", got, want)
		}
	})
}

// TestTheDefaultObserverIsInstalled keeps the production wiring honest: a
// breaker built without an observer still opens, and does so through the OTel
// counter rather than through nothing.
func TestTheDefaultObserverIsInstalled(t *testing.T) {
	t.Parallel()

	breaker := newTestBreaker(t, BreakerConfig{
		Name: readTool, FailureThreshold: 1, ResetTimeout: 10 * time.Second,
	})
	if breaker.onOpen == nil {
		t.Fatal("a breaker built without an observer has none")
	}
	failOnce(t, breaker)
	if got := breaker.State(); got != StateOpen {
		t.Errorf("State() = %s, want %s", got, StateOpen)
	}
}

// TestNewBreakerRejectsAnUnusableConfiguration is the fail-fast table. Both
// bounds are already enforced by the configuration package; a direct caller
// getting them wrong is a wiring bug that must not produce a breaker which
// silently never trips.
func TestNewBreakerRejectsAnUnusableConfiguration(t *testing.T) {
	t.Parallel()

	// Field order is go vet's fieldalignment choice, not a reading order.
	cases := []struct {
		name string
		want string
		cfg  BreakerConfig
	}{
		{"no name", "Name", BreakerConfig{FailureThreshold: 1, ResetTimeout: time.Second}},
		{"no threshold", "FailureThreshold", BreakerConfig{Name: readTool, ResetTimeout: time.Second}},
		{"negative threshold", "FailureThreshold", BreakerConfig{
			Name: readTool, FailureThreshold: -1, ResetTimeout: time.Second,
		}},
		{"no cooldown", "ResetTimeout", BreakerConfig{Name: readTool, FailureThreshold: 1}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			breaker, err := NewBreaker(testCase.cfg)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("NewBreaker() error = %v, want ErrInvalidConfig", err)
			}
			if breaker != nil {
				t.Error("NewBreaker() returned a breaker alongside its error")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("NewBreaker() error = %q, want it to name %s", err, testCase.want)
			}
		})
	}

	if _, err := NewBreakers(BreakersConfig{FailureThreshold: 0, ResetTimeout: time.Second}); !errors.Is(
		err, ErrInvalidConfig,
	) {
		t.Errorf("NewBreakers() error = %v, want the registry to validate the shared bounds", err)
	}
}

// TestArbitraryOutcomeSequencesPreserveTheGenerationContract is the port of
// test_circuit.py's hypothesis RuleBasedStateMachine.
//
// It drives permits, clock advances and outcomes in arbitrary order and asserts
// the two properties the generation counter exists to provide: a refused call is
// refused for a reason the caller can predict, and an outcome the breaker does
// not honor changes nothing at all. The step sequence is derived from a fixed
// set of seeds, so a failure is reproducible from the reported seed alone.
func TestArbitraryOutcomeSequencesPreserveTheGenerationContract(t *testing.T) {
	const (
		examples   = 50
		steps      = 30
		threshold  = 2
		cooldown   = 5 * time.Second
		maxAdvance = 10
	)

	for seed := range uint64(examples) {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				random := rand.New(rand.NewPCG(seed, 0x0a9e0f5))
				breaker := newTestBreaker(t, BreakerConfig{
					Name: readTool, FailureThreshold: threshold, ResetTimeout: cooldown,
				})
				var permits []Permit

				for step := range steps {
					before := breaker.snapshot()
					// An outcome needs a permit to name, so before the first one is
					// granted the only reachable action is to ask for one.
					action := random.IntN(5)
					if len(permits) == 0 && action >= 2 {
						action = 0
					}
					switch action {
					case 0:
						permit, ok := breaker.Allow()
						mustRefuse := before.state == StateHalfOpen ||
							(before.state == StateOpen && time.Since(before.openedAt) < cooldown)
						if mustRefuse && ok {
							t.Fatalf("step %d: Allow() admitted a call while %s", step, before.state)
						}
						if ok {
							permits = append(permits, permit)
						}
					case 1:
						time.Sleep(time.Duration(random.IntN(maxAdvance+1)) * time.Second)
					case 2:
						permit := permits[random.IntN(len(permits))]
						stale := permit.generation != before.generation || before.state == StateOpen
						breaker.RecordSuccess(permit)
						assertUnchangedWhenStale(t, step, stale, before, breaker.snapshot())
					case 3:
						permit := permits[random.IntN(len(permits))]
						stale := before.state == StateOpen || permit.generation != before.generation
						breaker.RecordFailure(context.Background(), permit)
						assertUnchangedWhenStale(t, step, stale, before, breaker.snapshot())
					case 4:
						permit := permits[random.IntN(len(permits))]
						stale := permit.generation != before.generation ||
							before.state != StateHalfOpen || !before.probeActive
						breaker.RecordAbandoned(context.Background(), permit)
						assertUnchangedWhenStale(t, step, stale, before, breaker.snapshot())
					}

					after := breaker.snapshot()
					for _, permit := range permits {
						if permit.generation > after.generation {
							t.Fatalf("step %d: a permit claims generation %d, ahead of the breaker's %d",
								step, permit.generation, after.generation)
						}
					}
					if after.failures < 0 {
						t.Fatalf("step %d: failures = %d", step, after.failures)
					}
					if after.generation < before.generation {
						t.Fatalf("step %d: generation went backwards, %d to %d",
							step, before.generation, after.generation)
					}
				}
			})
		})
	}
}

// assertUnchangedWhenStale is the state machine's core invariant: an outcome the
// breaker does not honor must leave every observable field exactly as it was.
func assertUnchangedWhenStale(t *testing.T, step int, stale bool, before, after breakerState) {
	t.Helper()

	if !stale || before == after {
		return
	}
	t.Fatalf("step %d: a stale outcome changed the breaker from %+v to %+v", step, before, after)
}
