package resilience

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// This file is the Go port of tests/test_resilience.py.
//
// Two Python cases have no Go home and are called out where they belong:
// test_wrapper_preserves_tool_schema_inputs asserted that functools.wraps kept
// the signature ADK reads, which in Go is decided by the tool's own types and
// cannot be disturbed by a guard at all — what survives of it is
// TestRunSatisfiesTheToolSeam below, which pins the seam's shape instead.
// test_guarded_actions_are_never_wrapped asserts a property of the tools
// package, and lives there as TestGuardedWritesNeverRunThroughTheResilienceGuard.
//
// Everything that depends on time runs inside testing/synctest. Its virtualized
// clock is what makes a backoff schedule an exact assertion rather than a
// tolerance, and it is why no test here sleeps for real.

// toolGuard mirrors the seam the tools package declares for a guarded read. It
// is redeclared rather than imported so this package keeps no dependency on its
// own consumer, while still failing to compile if the two shapes ever drift.
type toolGuard func(ctx context.Context, toolName string, call func(context.Context) error) error

// The resources these tests guard. They are deliberately generic: the policy is
// about failure shapes, not about any particular tool, and the ratchet in
// domain/portability_test.go keeps seed identifiers out of every package but
// domain.
const (
	readTool  = "reads"
	probeTool = "probe"
)

// errTransient is the retryable fault every case here fails with.
var errTransient = errors.New("dependency is unavailable")

// newTestGuard builds a guard and fails the test rather than returning an error,
// because a guard that will not build is a bug in the case, not a result.
//
// It silences the policy's own warning and error lines. That weakens nothing:
// every assertion below is about behavior, and a suite that printed a warning
// per retry would bury the failures that matter.
func newTestGuard(t *testing.T, cfg Config) *Guard {
	t.Helper()

	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	guard, err := NewGuard(cfg)
	if err != nil {
		t.Fatalf("NewGuard() error = %v, want nil", err)
	}
	return guard
}

// TestRunSatisfiesTheToolSeam is what remains of
// test_wrapper_preserves_tool_schema_inputs: the guarantee is no longer "the
// decorator preserved the signature" but "the guard fits the seam the tools
// package declares", which the compiler checks here and nowhere else.
func TestRunSatisfiesTheToolSeam(t *testing.T) {
	t.Parallel()

	guard := newTestGuard(t, Config{ToolTimeout: time.Second})
	var seam toolGuard = guard.Run

	calls := 0
	if err := seam(t.Context(), readTool, func(context.Context) error {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if calls != 1 {
		t.Errorf("the tool ran %d times, want 1", calls)
	}
}

// TestTheDefaultLoggerIsResolvedWhenTheGuardLogs covers runtime assembly:
// the policy redactor has to exist before the sanitizing default logger can be
// installed, so a guard built earlier must not retain the raw startup logger.
func TestTheDefaultLoggerIsResolvedWhenTheGuardLogs(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })

	var startup, installed bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&startup, nil)))
	guard, err := NewGuard(Config{
		ToolTimeout: time.Second,
		MaxRetries:  1,
	})
	if err != nil {
		t.Fatalf("NewGuard() error = %v, want nil", err)
	}

	// This stands in for cmd/agent installing its redacting handler after the
	// policy plane is available but before any tool call can run.
	slog.SetDefault(slog.New(slog.NewTextHandler(&installed, nil)))
	const untrusted = "password=SYNTHETIC_DO_NOT_USE_RETRY_LOG_123456"
	if err := guard.Run(t.Context(), readTool, func(context.Context) error {
		return errors.New(untrusted)
	}); err == nil {
		t.Fatal("Run() error = nil, want the exhausted retry failure")
	}

	if strings.Contains(startup.String(), untrusted) {
		t.Fatalf("the pre-install logger retained the dependency error: %q", startup.String())
	}
	if !strings.Contains(installed.String(), untrusted) {
		t.Fatalf("the installed logger never received the retry record: %q", installed.String())
	}
}

// TestAnAttemptRunsUnderItsOwnDeadline pins the shape of the context the tool
// body receives. It is the mechanism the whole package rests on: a deadline that
// never reached the callee would bound nothing.
func TestAnAttemptRunsUnderItsOwnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		guard := newTestGuard(t, Config{ToolTimeout: 30 * time.Second})

		var remaining time.Duration
		if err := guard.Run(context.Background(), readTool, func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Error("the attempt context carries no deadline")
				return nil
			}
			remaining = time.Until(deadline)
			return nil
		}); err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		if remaining != 30*time.Second {
			t.Errorf("the attempt had %s left, want the full 30s budget", remaining)
		}
	})
}

// TestATransientFailureRecoversWithinTheRetryBudget is the port of
// test_transient_failure_recovers: one hiccup costs a retry, not a turn.
func TestATransientFailureRecoversWithinTheRetryBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		guard := newTestGuard(t, Config{
			ToolTimeout:  30 * time.Second,
			MaxRetries:   2,
			RetryBackoff: 500 * time.Millisecond,
		})

		calls := 0
		err := guard.Run(context.Background(), readTool, func(context.Context) error {
			calls++
			if calls == 1 {
				return errTransient
			}
			return nil
		})
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		if calls != 2 {
			t.Errorf("the tool ran %d times, want 2", calls)
		}
	})
}

// TestExhaustedRetriesSurfaceTheRootCause is the port of
// test_permanent_failure_surfaces_with_context. Python preserved the cause on
// __cause__; Go preserves it through Unwrap, so errors.Is still classifies it.
func TestExhaustedRetriesSurfaceTheRootCause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		guard := newTestGuard(t, Config{
			ToolTimeout:  30 * time.Second,
			MaxRetries:   1,
			RetryBackoff: 500 * time.Millisecond,
		})

		err := guard.Run(context.Background(), readTool, func(context.Context) error {
			return errTransient
		})

		var exhausted *RetriesExhaustedError
		if !errors.As(err, &exhausted) {
			t.Fatalf("Run() error = %v, want a *RetriesExhaustedError", err)
		}
		if exhausted.Attempts != 2 {
			t.Errorf("Attempts = %d, want max_retries + 1 = 2", exhausted.Attempts)
		}
		if exhausted.Tool != readTool {
			t.Errorf("Tool = %q, want %q", exhausted.Tool, readTool)
		}
		if !errors.Is(err, errTransient) {
			t.Errorf("Run() error = %v, want the root cause preserved", err)
		}
	})
}

// TestBackoffDoublesAndStopsBeforeTheLastAttempt is the port of
// test_backoff_grows_exponentially. Python asserted the sleeps a fake recorded;
// under a virtualized clock the real timers can be asserted instead, which also
// proves the schedule is what actually elapses.
func TestBackoffDoublesAndStopsBeforeTheLastAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		guard := newTestGuard(t, Config{
			ToolTimeout:  30 * time.Second,
			MaxRetries:   2,
			RetryBackoff: 500 * time.Millisecond,
		})

		start := time.Now()
		var offsets []time.Duration
		err := guard.Run(context.Background(), readTool, func(context.Context) error {
			offsets = append(offsets, time.Since(start))
			return errTransient
		})
		if err == nil {
			t.Fatal("Run() error = nil, want the retries to be exhausted")
		}

		// 0.5s before the second attempt and 1.0s before the third, exactly as the
		// Python suite recorded, expressed as the instants the attempts happen.
		want := []time.Duration{0, 500 * time.Millisecond, 1500 * time.Millisecond}
		if len(offsets) != len(want) {
			t.Fatalf("the tool ran %d times at %v, want 3", len(offsets), offsets)
		}
		for i, offset := range offsets {
			if offset != want[i] {
				t.Errorf("attempt %d ran at %s, want %s", i+1, offset, want[i])
			}
		}
		// Nothing waits after the last attempt: there is nothing left to wait for.
		if elapsed := time.Since(start); elapsed != 1500*time.Millisecond {
			t.Errorf("Run() took %s, want 1.5s — a wait after the last attempt is pure latency", elapsed)
		}
	})
}

// TestADeadlineIsNeverRetried is the port of test_deadline_raises_without_retry.
// A deadline is a budget, not a blip: the next attempt would burn the same one.
func TestADeadlineIsNeverRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		guard := newTestGuard(t, Config{
			ToolTimeout:  5 * time.Second,
			MaxRetries:   3,
			RetryBackoff: time.Second,
		})

		start := time.Now()
		calls := 0
		err := guard.Run(context.Background(), readTool, func(ctx context.Context) error {
			calls++
			<-ctx.Done()
			return ctx.Err()
		})

		var deadline *DeadlineError
		if !errors.As(err, &deadline) {
			t.Fatalf("Run() error = %v, want a *DeadlineError", err)
		}
		if deadline.Timeout != 5*time.Second {
			t.Errorf("Timeout = %s, want the configured 5s budget", deadline.Timeout)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Error("a tool deadline must still classify as context.DeadlineExceeded")
		}
		if calls != 1 {
			t.Errorf("the tool ran %d times, want 1", calls)
		}
		if elapsed := time.Since(start); elapsed != 5*time.Second {
			t.Errorf("Run() took %s, want exactly one 5s budget", elapsed)
		}
	})
}

// TestACancelledCallerAbandonsTheBackoffImmediately is the guarantee Python's
// asyncio.CancelledError path bought and the one requirement a hand-rolled sleep
// loop would lose: a turn nobody is waiting for must not be kept alive by a
// timer.
func TestACancelledCallerAbandonsTheBackoffImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		guard := newTestGuard(t, Config{
			ToolTimeout:  time.Hour,
			MaxRetries:   5,
			RetryBackoff: 10 * time.Second,
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		start := time.Now()
		calls := 0
		done := make(chan error, 1)
		go func() {
			done <- guard.Run(ctx, readTool, func(context.Context) error {
				calls++
				return errTransient
			})
		}()

		// The run is now parked in its first backoff, with ten virtual seconds to
		// go and nothing else to do.
		synctest.Wait()
		cancel()

		err := <-done
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want it to carry context.Canceled", err)
		}
		if calls != 1 {
			t.Errorf("the tool ran %d times, want 1 — a canceled caller gets no further attempt", calls)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Errorf("Run() waited %s after the cancellation, want none", elapsed)
		}
	})
}

// TestAnAlreadyCancelledCallerNeverReachesTheDependency covers the other end of
// the same rule: a turn that was abandoned before the call started must not
// produce load.
func TestAnAlreadyCancelledCallerNeverReachesTheDependency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		guard := newTestGuard(t, Config{ToolTimeout: 30 * time.Second, MaxRetries: 2})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := guard.Run(ctx, readTool, func(ctx context.Context) error { return ctx.Err() })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want it to carry context.Canceled", err)
		}
	})
}

// TestDoCarriesTheValueOfTheSuccessfulAttempt covers the generic helper the
// tools compose with. The zero value on failure is the load-bearing half: a
// partial result written by a failed attempt must never escape as if it were an
// answer.
func TestDoCarriesTheValueOfTheSuccessfulAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		guard := newTestGuard(t, Config{
			ToolTimeout:  30 * time.Second,
			MaxRetries:   2,
			RetryBackoff: 500 * time.Millisecond,
		})

		calls := 0
		value, err := Do(context.Background(), guard, readTool, func(context.Context) (int, error) {
			calls++
			if calls == 1 {
				return -1, errTransient
			}
			return 42, nil
		})
		if err != nil {
			t.Fatalf("Do() error = %v, want nil", err)
		}
		if value != 42 {
			t.Errorf("Do() = %d, want the successful attempt's 42", value)
		}

		partial, err := Do(context.Background(), guard, readTool, func(context.Context) (int, error) {
			return -1, errTransient
		})
		if err == nil {
			t.Fatal("Do() error = nil, want the retries to be exhausted")
		}
		if partial != 0 {
			t.Errorf("Do() = %d on failure, want the zero value", partial)
		}
	})
}

// TestDoRefusesAMissingGuard keeps the wiring bug loud. A nil guard is a
// composition that silently dropped a Chapter 4.5 guarantee, and silence is
// exactly the wrong failure mode for a control.
func TestDoRefusesAMissingGuard(t *testing.T) {
	t.Parallel()

	calls := 0
	_, err := Do(t.Context(), nil, readTool, func(context.Context) (int, error) {
		calls++
		return 1, nil
	})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Do() error = %v, want ErrInvalidConfig", err)
	}
	if calls != 0 {
		t.Errorf("the tool ran %d times without a guard, want 0", calls)
	}
}

// TestNewGuardRejectsAnUnusableConfiguration is the fail-fast table. Each of
// these would degrade a guarantee rather than fail visibly at runtime.
func TestNewGuardRejectsAnUnusableConfiguration(t *testing.T) {
	t.Parallel()

	// Field order is go vet's fieldalignment choice, not a reading order.
	cases := []struct {
		name string
		want string
		cfg  Config
	}{
		{"no deadline", "ToolTimeout", Config{ToolTimeout: 0}},
		{"negative deadline", "ToolTimeout", Config{ToolTimeout: -time.Second}},
		{"negative retries", "MaxRetries", Config{ToolTimeout: time.Second, MaxRetries: -1}},
		{"negative backoff", "RetryBackoff", Config{ToolTimeout: time.Second, RetryBackoff: -time.Second}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			guard, err := NewGuard(testCase.cfg)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("NewGuard() error = %v, want ErrInvalidConfig", err)
			}
			if guard != nil {
				t.Error("NewGuard() returned a guard alongside its error")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("NewGuard() error = %q, want it to name %s", err, testCase.want)
			}
		})
	}
}

// TestAttemptsIsOneMoreThanTheRetryBudget pins the arithmetic the two error
// messages and the breaker accounting both depend on.
func TestAttemptsIsOneMoreThanTheRetryBudget(t *testing.T) {
	t.Parallel()

	for retries, want := range map[int]int{0: 1, 1: 2, 2: 3, 10: 11} {
		guard := newTestGuard(t, Config{ToolTimeout: time.Second, MaxRetries: retries})
		if got := guard.Attempts(); got != want {
			t.Errorf("Attempts() with MaxRetries=%d = %d, want %d", retries, got, want)
		}
	}
}

// TestBackoffSaturatesInsteadOfWrapping guards the one arithmetic trap in the
// schedule. A time.Duration is int64 nanoseconds, so a doubling that overflowed
// would produce a negative delay — no wait at all — and quietly turn a backoff
// into a hot retry loop against a dependency that is already down.
func TestBackoffSaturatesInsteadOfWrapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		base    time.Duration
		attempt int
		want    time.Duration
	}{
		{500 * time.Millisecond, 0, 500 * time.Millisecond},
		{500 * time.Millisecond, 1, time.Second},
		{500 * time.Millisecond, 2, 2 * time.Second},
		{500 * time.Millisecond, 3, 4 * time.Second},
		{0, 5, 0},
		{time.Second, 64, math.MaxInt64},
	}
	for _, testCase := range cases {
		got := backoffFor(testCase.base, testCase.attempt)
		if got != testCase.want {
			t.Errorf("backoffFor(%s, %d) = %s, want %s", testCase.base, testCase.attempt, got, testCase.want)
		}
		if got < 0 {
			t.Errorf("backoffFor(%s, %d) is negative, which would skip the wait entirely",
				testCase.base, testCase.attempt)
		}
	}
}
