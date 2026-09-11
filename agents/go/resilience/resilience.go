// Package resilience bounds what one failing dependency can cost a turn.
//
// It carries two complementary levers, ported from the Python track's
// resilience.py and circuit.py:
//
//   - A [Guard] gives every idempotent read a per-attempt deadline and bounded
//     retries with exponential backoff, so a transient fault — an Ollama cold
//     start, a gateway restart, a locked SQLite file — costs a retry instead of
//     a failed turn.
//   - A [Breaker] stops those retries from becoming the problem. When a
//     dependency is persistently down, every turn would otherwise pay the whole
//     retry budget before failing. After a run of failures the breaker opens and
//     fails fast, then after a cooldown admits exactly one trial call.
//
// Both only ever wrap idempotent reads. The two guarded write actions
// deliberately never go through a Guard: an automatic retry can cross an unknown
// commit boundary and apply the write twice. The tools package enforces that
// split by taking the guard as an explicit seam its reads use and its writes
// do not.
//
// # The context is the deadline
//
// Python ran each attempt in a worker thread so asyncio.wait_for could stop the
// turn from waiting; it could not cancel the thread, so a timed-out read might
// still complete in the background. Go has the stronger primitive: each attempt
// runs under its own context.WithTimeout, so a callee that honors ctx — every
// database/sql query and net/http request does — is genuinely canceled.
//
// That moves the contract onto the callee. A call that ignores its context
// cannot be bounded by anything this package does, and no goroutine is spawned
// to pretend otherwise: abandoning a running call would leak it, which is the
// very flaw the Python docstring had to warn about.
//
// A canceled parent context is never treated as a dependency failure. It
// abandons the wait immediately, releases the breaker probe without counting a
// failure, and returns the context's own error.
package resilience

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"
)

// ErrInvalidConfig marks a configuration that cannot produce a working guard or
// breaker.
//
// It is a sentinel so a caller can tell a wiring bug — which is a startup
// failure and should stop the process — apart from a runtime one.
var ErrInvalidConfig = errors.New("invalid resilience configuration")

// DeadlineError reports that one tool call exhausted its per-attempt budget.
//
// It unwraps to context.DeadlineExceeded, which is the Go counterpart of the
// Python type deriving from TimeoutError: code that only asks "was this a
// timeout" keeps working through errors.Is without knowing this package exists.
type DeadlineError struct {
	// Tool is the read that ran out of time.
	Tool string
	// Timeout is the budget it exceeded.
	Timeout time.Duration
}

func (e *DeadlineError) Error() string {
	// AGENT_TOOL_TIMEOUT_S is named literally rather than read from the config
	// package: this package deliberately depends on no other in the module, and
	// the variable name is operator-facing prose, not a value anything computes
	// with.
	return fmt.Sprintf("tool %q exceeded its %s deadline (AGENT_TOOL_TIMEOUT_S)", e.Tool, e.Timeout)
}

// Unwrap makes errors.Is(err, context.DeadlineExceeded) true for a tool deadline.
func (e *DeadlineError) Unwrap() error { return context.DeadlineExceeded }

// RetriesExhaustedError reports that every attempt allowed by the retry budget
// failed.
type RetriesExhaustedError struct {
	// Err is the failure of the last attempt. It is preserved rather than
	// summarized, the way the Python original chained the root cause onto
	// __cause__, so a caller can still classify what actually went wrong.
	Err error
	// Tool is the read that never succeeded.
	Tool string
	// Attempts is how many calls were made, which is MaxRetries + 1.
	Attempts int
}

func (e *RetriesExhaustedError) Error() string {
	return fmt.Sprintf(
		"tool %q failed after %d attempts (AGENT_MAX_RETRIES): %v", e.Tool, e.Attempts, e.Err,
	)
}

// SafeSummary describes the retry policy without retaining the final
// dependency error, which may contain a provider body, credential, or endpoint.
func (e *RetriesExhaustedError) SafeSummary() string {
	return fmt.Sprintf(
		"tool %q failed after %d attempts (AGENT_MAX_RETRIES)", e.Tool, e.Attempts,
	)
}

// Unwrap exposes the root cause.
func (e *RetriesExhaustedError) Unwrap() error { return e.Err }

// Config is everything a [Guard] needs from the resolved configuration.
//
// It is a small struct of its own rather than config.Config, for the same reason
// the tools package declares its own store interface: the consumer states
// exactly what it depends on, a reader sees the whole surface at once, and a
// test exercises a policy without building an environment. The composition root
// converts once.
type Config struct {
	// Breakers gates every call behind a per-tool circuit breaker. Nil is the
	// shipped default (AGENT_CIRCUIT_BREAKER_ENABLED=false): retry only, and no
	// breaker is ever created for any tool.
	Breakers *Breakers
	// Logger receives the retry, deadline and open-circuit lines. Nil resolves
	// slog.Default when a record is emitted, so a guard built before the runtime
	// installs its sanitizing handler cannot retain the raw startup logger.
	Logger *slog.Logger
	// ToolTimeout bounds one attempt (AGENT_TOOL_TIMEOUT_S).
	ToolTimeout time.Duration
	// RetryBackoff is the delay before the second attempt
	// (AGENT_RETRY_BACKOFF_S); every later delay doubles it.
	RetryBackoff time.Duration
	// MaxRetries is the number of retries *after* the first attempt
	// (AGENT_MAX_RETRIES), so one call makes MaxRetries+1 attempts.
	MaxRetries int
}

// Guard runs idempotent reads under a deadline, bounded retries, and — when a
// registry is configured — a per-tool circuit breaker.
//
// It is the Go shape of Python's with_resilience decorator. A decorator could
// not survive the port: Go tools are typed functions whose schema the framework
// infers from those types, so wrapping one in a generic decorator would erase
// the very signature ADK reads. Passing the policy as a seam the tool calls
// instead keeps the schema intact and makes the policy substitutable in a test.
//
// The zero value is not usable; build one with [NewGuard].
type Guard struct {
	breakers     *Breakers
	log          *slog.Logger
	toolTimeout  time.Duration
	retryBackoff time.Duration
	maxRetries   int
}

// NewGuard builds a guard, refusing a configuration that would silently disable
// a Chapter 4.5 guarantee.
func NewGuard(cfg Config) (*Guard, error) {
	var problems []error
	if cfg.ToolTimeout <= 0 {
		problems = append(problems, fmt.Errorf(
			"%w: ToolTimeout is %s, want a positive duration; a read with no deadline can hang a turn forever",
			ErrInvalidConfig, cfg.ToolTimeout,
		))
	}
	if cfg.MaxRetries < 0 {
		problems = append(problems, fmt.Errorf(
			"%w: MaxRetries is %d, want zero or more", ErrInvalidConfig, cfg.MaxRetries,
		))
	}
	if cfg.RetryBackoff < 0 {
		problems = append(problems, fmt.Errorf(
			"%w: RetryBackoff is %s, want zero or more", ErrInvalidConfig, cfg.RetryBackoff,
		))
	}
	if err := errors.Join(problems...); err != nil {
		return nil, err
	}

	return &Guard{
		breakers:     cfg.Breakers,
		log:          cfg.Logger,
		toolTimeout:  cfg.ToolTimeout,
		retryBackoff: cfg.RetryBackoff,
		maxRetries:   cfg.MaxRetries,
	}, nil
}

// logger returns the explicitly injected logger, or the process default that
// is active now. Runtime assembly installs the sanitizing default only after it
// has built the policy redactor, so resolving a nil logger in NewGuard would
// capture the unsanitized startup handler.
func (g *Guard) logger() *slog.Logger {
	if g.log != nil {
		return g.log
	}
	return slog.Default()
}

// Breakers returns the circuit-breaker registry, or nil when breaking is off.
//
// Nil is the observable form of AGENT_CIRCUIT_BREAKER_ENABLED=false: there is no
// registry to hold a breaker, which is a stronger guarantee than an empty one.
func (g *Guard) Breakers() *Breakers { return g.breakers }

// Attempts returns how many calls one guarded read may make, MaxRetries + 1.
func (g *Guard) Attempts() int { return g.maxRetries + 1 }

// Run executes one idempotent read under the full policy.
//
// The signature is the seam the tools package declares, so a *Guard's Run method
// value is directly assignable to it. toolName is passed through because the
// breaker is per tool: one failing dependency must not shed load from the rest.
//
// call must be safe to run more than once. It receives a context carrying the
// attempt's deadline, and it must honor it — see the package documentation.
func (g *Guard) Run(ctx context.Context, toolName string, call func(context.Context) error) error {
	admitted, err := g.admit(ctx, toolName)
	if err != nil {
		return err
	}

	attempts := g.Attempts()
	var lastErr error
	for attempt := range attempts {
		// A fresh deadline per attempt, exactly as Python wrapped each attempt in
		// its own wait_for: the budget is what one call may take, not what the
		// whole retry sequence may take.
		attemptCtx, cancel := context.WithTimeout(ctx, g.toolTimeout)
		callErr := call(attemptCtx)
		expired := errors.Is(attemptCtx.Err(), context.DeadlineExceeded)
		cancel()

		switch {
		case callErr == nil:
			admitted.success()
			return nil

		case ctx.Err() != nil:
			// The caller stopped waiting. That is not evidence about the
			// dependency, so the probe is released without counting a failure.
			admitted.abandoned(ctx)
			return fmt.Errorf("tool %q was abandoned before it finished: %w", toolName, ctx.Err())

		case expired:
			// A deadline is a budget, not a transient blip: the next attempt would
			// most likely burn the same budget, so it is never retried.
			g.logger().ErrorContext(ctx, "tool exceeded its deadline",
				"tool", toolName, "timeout", g.toolTimeout)
			admitted.failure(ctx)
			return &DeadlineError{Tool: toolName, Timeout: g.toolTimeout}
		}

		lastErr = callErr
		if attempt == attempts-1 {
			// No wait after the last attempt: nothing follows it to wait for.
			break
		}
		delay := backoffFor(g.retryBackoff, attempt)
		g.logger().WarnContext(ctx, "tool failed, retrying after backoff",
			"tool", toolName, "attempt", attempt+1, "attempts", attempts,
			"delay", delay, "error", callErr)
		if err := wait(ctx, delay); err != nil {
			admitted.abandoned(ctx)
			return fmt.Errorf("tool %q was abandoned while backing off: %w", toolName, err)
		}
	}

	// One breaker failure for the whole sequence, not one per attempt: the
	// threshold counts failing *calls*, and counting attempts instead would make
	// it trip MaxRetries+1 times faster than its configured value says.
	admitted.failure(ctx)
	return &RetriesExhaustedError{Err: lastErr, Tool: toolName, Attempts: attempts}
}

// Do runs an idempotent read that produces a value, under the full policy.
//
// It exists because Go methods cannot carry type parameters while [Guard.Run]'s
// signature has to stay exactly the seam the tools package declares. Do keeps
// the closure-over-a-local dance in one place instead of repeating it at every
// call site, and returns the zero value on failure so a partial result from a
// failed attempt can never escape.
func Do[T any](
	ctx context.Context, guard *Guard, toolName string, call func(context.Context) (T, error),
) (T, error) {
	var value T
	if guard == nil {
		return value, fmt.Errorf(
			"%w: a guard is required; an unguarded read has no deadline, no retries and no breaker",
			ErrInvalidConfig,
		)
	}
	err := guard.Run(ctx, toolName, func(ctx context.Context) error {
		var callErr error
		value, callErr = call(ctx)
		return callErr
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return value, nil
}

// admission is one breaker permit, or the no-op admission the retry-only default
// uses. Bundling the breaker with its permit is what keeps a nil check out of
// every outcome path in [Guard.Run].
type admission struct {
	breaker *Breaker
	permit  Permit
}

func (a admission) success() {
	if a.breaker != nil {
		a.breaker.RecordSuccess(a.permit)
	}
}

func (a admission) failure(ctx context.Context) {
	if a.breaker != nil {
		a.breaker.RecordFailure(ctx, a.permit)
	}
}

func (a admission) abandoned(ctx context.Context) {
	if a.breaker != nil {
		a.breaker.RecordAbandoned(ctx, a.permit)
	}
}

// admit asks the tool's breaker for permission before any attempt is made.
//
// An open breaker short-circuits the whole retry loop rather than the first
// attempt: a dependency that just failed its budget should not be hammered
// through another full budget.
func (g *Guard) admit(ctx context.Context, toolName string) (admission, error) {
	if g.breakers == nil {
		return admission{}, nil
	}
	breaker := g.breakers.Get(toolName)
	permit, ok := breaker.Allow()
	if !ok {
		g.logger().WarnContext(ctx, "tool circuit is open, failing fast without a call", "tool", toolName)
		return admission{}, &CircuitOpenError{Tool: toolName, ResetTimeout: breaker.ResetTimeout()}
	}
	return admission{breaker: breaker, permit: permit}, nil
}

// backoffFor returns the delay before the attempt after this one:
// RetryBackoff × 2^attempt, with attempt counted from zero.
//
// The doubling saturates instead of wrapping. A time.Duration is int64
// nanoseconds, and a shift past that range would produce a negative delay — that
// is, no wait at all — turning an over-configured backoff into a hot retry loop.
func backoffFor(base time.Duration, attempt int) time.Duration {
	delay := base
	for range attempt {
		if delay > math.MaxInt64/2 {
			return math.MaxInt64
		}
		delay *= 2
	}
	return delay
}

// wait blocks for delay, or returns as soon as ctx is done.
//
// This is the whole reason a retry loop may not sleep: a turn the caller already
// abandoned must not be kept alive by a timer nobody is waiting for.
func wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
