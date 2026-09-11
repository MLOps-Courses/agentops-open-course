package resilience

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// State is one of the three states of a standard circuit breaker.
//
// It is a defined type rather than a bare string so a state can never be
// compared against a typo: the compiler rejects the literal the Python StrEnum
// only caught at runtime.
type State string

const (
	// StateClosed lets calls through and counts consecutive failures.
	StateClosed State = "closed"
	// StateOpen fails every call fast until the cooldown elapses.
	StateOpen State = "open"
	// StateHalfOpen admits exactly one trial call to test recovery.
	StateHalfOpen State = "half_open"
)

// meterName is the instrumentation scope every agent metric shares.
const meterName = "agentops.agent"

// openedCounter counts breaker openings, labeled by the resource that opened.
//
// A counter rather than a gauge, because the alert that matters is on an opening
// *rate*: a breaker that flaps open repeatedly is a louder signal than one that
// merely happens to be open when a scrape lands. Built once and lazily, because
// the global meter provider is a no-op until the launcher installs the real one
// and the delegating instrument picks that up on its own.
var openedCounter = sync.OnceValue(func() metric.Int64Counter {
	counter, err := otel.Meter(meterName).Int64Counter(
		"agentops.circuit.opened_total",
		metric.WithUnit("1"),
		metric.WithDescription("Times a circuit breaker opened, by resource"),
	)
	if err != nil {
		// The OTel API returns a working no-op instrument alongside the error, so
		// the breaker keeps protecting the dependency. Losing a metric must never
		// break the control it measures.
		slog.Warn("the circuit breaker opening counter is unavailable", "error", err)
	}
	return counter
})

// recordOpened is the default open observer: the OTel counter the Python track
// exposes under the same name, unit, description and attribute key.
func recordOpened(ctx context.Context, name string) {
	openedCounter().Add(ctx, 1, metric.WithAttributes(attribute.String("resource", name)))
}

// CircuitOpenError is returned instead of calling a dependency whose breaker is
// open.
type CircuitOpenError struct {
	// Tool is the resource whose breaker refused the call.
	Tool string
	// ResetTimeout is the longest a caller has to wait before one trial call is
	// admitted again.
	ResetTimeout time.Duration
}

func (e *CircuitOpenError) Error() string {
	// AGENT_CIRCUIT_RESET_TIMEOUT_S is named literally rather than read from the
	// config package: this package deliberately depends on no other, and the
	// variable name is operator-facing prose, not a value anything computes with.
	return fmt.Sprintf(
		"tool %q circuit is open after repeated failures; retrying in at most %s (AGENT_CIRCUIT_RESET_TIMEOUT_S)",
		e.Tool, e.ResetTimeout,
	)
}

// Permit is the breaker generation in which one dependency call was admitted.
//
// Outcome methods honor only permits from the current generation, so a slow
// call admitted before a newer failure opened the breaker cannot later close or
// re-open it. That is the whole point of the type.
//
// The zero value was never admitted, and every outcome method ignores it. This
// is what the (Permit, bool) result buys over Python's optional permit: there is
// no way to spell an outcome for a call the breaker refused, and no nil to
// dereference.
type Permit struct {
	generation uint64
	admitted   bool
}

// BreakerConfig describes one per-resource breaker.
type BreakerConfig struct {
	// OnOpen observes every closed/half-open → open transition. Nil installs the
	// OTel counter. It runs while the breaker's lock is held, so it must not call
	// back into the breaker.
	OnOpen func(ctx context.Context, name string)
	// Name is the resource the breaker protects — a tool name, in this course.
	Name string
	// FailureThreshold is the number of consecutive failures that opens the
	// breaker (AGENT_CIRCUIT_FAILURE_THRESHOLD).
	FailureThreshold int
	// ResetTimeout is how long the breaker stays open before one trial call is
	// admitted (AGENT_CIRCUIT_RESET_TIMEOUT_S).
	ResetTimeout time.Duration
}

// Breaker is a per-resource circuit breaker.
//
// After FailureThreshold consecutive failures it opens and sheds load off the
// struggling dependency; after ResetTimeout it admits exactly one trial call,
// which either closes it again or re-opens it with a fresh cooldown.
//
// Unlike the Python original the clock is not injectable. Go has a better tool
// for the job: testing/synctest virtualizes time for the whole bubble, so the
// same time.Now the production path uses is already deterministic under test —
// and so are the retry timers in [Guard], which an injected clock would not have
// reached.
type Breaker struct {
	// Field order here is what go vet's fieldalignment check wants — Go lays a
	// struct out in declaration order — and not how the fields group by meaning.
	// The grouping that matters when reading this type is: onOpen, name,
	// failureThreshold and resetTimeout are immutable after construction, and
	// everything else is guarded by mu.
	//
	// openedAt is when the current open period started, which the cooldown is
	// measured from; probeActive marks the one half-open trial call in flight.
	openedAt         time.Time
	onOpen           func(ctx context.Context, name string)
	name             string
	state            State
	failureThreshold int
	resetTimeout     time.Duration
	failures         int
	generation       uint64
	mu               sync.Mutex
	probeActive      bool
}

// NewBreaker builds one breaker, refusing a configuration it cannot honor.
func NewBreaker(cfg BreakerConfig) (*Breaker, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("%w: Name is required; a breaker is per resource", ErrInvalidConfig)
	}
	if err := validateBreakerLimits(cfg.FailureThreshold, cfg.ResetTimeout); err != nil {
		return nil, err
	}
	return newBreaker(cfg), nil
}

// validateBreakerLimits checks the two bounds a breaker cannot work without.
//
// It is shared with [NewBreakers] so the registry rejects an impossible
// configuration once, at startup, instead of on whichever tool call happens to
// be the first to need a breaker.
func validateBreakerLimits(failureThreshold int, resetTimeout time.Duration) error {
	switch {
	case failureThreshold < 1:
		return fmt.Errorf(
			"%w: FailureThreshold is %d, want at least 1; a breaker that opens on no failures never closes",
			ErrInvalidConfig, failureThreshold,
		)
	case resetTimeout <= 0:
		return fmt.Errorf(
			"%w: ResetTimeout is %s, want a positive duration; a breaker with no cooldown never sheds load",
			ErrInvalidConfig, resetTimeout,
		)
	}
	return nil
}

// newBreaker builds a breaker whose configuration is already known to be valid,
// which is what lets [Breakers.Get] stay total.
func newBreaker(cfg BreakerConfig) *Breaker {
	if cfg.OnOpen == nil {
		cfg.OnOpen = recordOpened
	}
	return &Breaker{
		onOpen:           cfg.OnOpen,
		name:             cfg.Name,
		failureThreshold: cfg.FailureThreshold,
		resetTimeout:     cfg.ResetTimeout,
		state:            StateClosed,
	}
}

// Name returns the resource this breaker protects.
func (b *Breaker) Name() string { return b.name }

// ResetTimeout returns how long the breaker stays open before a trial call.
func (b *Breaker) ResetTimeout() time.Duration { return b.resetTimeout }

// State returns the breaker's current state.
func (b *Breaker) State() State { return b.snapshot().state }

// Allow admits one call, advancing open → half-open once the cooldown elapsed.
//
// Closed admits concurrent callers; open admits exactly one, and only after the
// cooldown; half-open admits none while its trial call is still in flight. A
// false second result means the caller must fail fast.
func (b *Breaker) Allow() (Permit, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateOpen:
		// Strictly less than, so the probe is admitted at exactly the cooldown
		// rather than one tick after it.
		if time.Since(b.openedAt) < b.resetTimeout {
			return Permit{}, false
		}
		b.state = StateHalfOpen
		// The bump retires every permit admitted before this recovery attempt: a
		// straggler from the failed era must not be able to close the breaker.
		b.generation++
		b.probeActive = true
	case StateHalfOpen:
		if b.probeActive {
			return Permit{}, false
		}
		b.probeActive = true
	case StateClosed:
		// Closed is unbounded: only the failure count is shared, and counting it
		// is what the threshold is for.
	}
	return Permit{generation: b.generation, admitted: true}, true
}

// RecordSuccess records a current-generation success, ignoring stale outcomes.
//
// It takes no context because it emits no telemetry: a success is the absence of
// an event, and the counter this package exports measures openings.
func (b *Breaker) RecordSuccess(permit Permit) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.honors(permit) || b.state == StateOpen {
		return
	}
	if b.state == StateHalfOpen {
		b.state = StateClosed
		// Recovery retires the probe's own generation, so a failure from the open
		// era that lands afterwards cannot re-open the breaker it just closed.
		b.generation++
	}
	b.failures = 0
	b.probeActive = false
}

// RecordFailure records a current-generation failure and trips at the threshold.
//
// A failure while half-open re-opens immediately whatever the count says: the
// trial call just proved the dependency is still unhealthy, so the cooldown
// restarts from now. A stale outcome, or one arriving while the breaker is
// already open, is a no-op — it must not change the failure count or the
// cooldown that is already running.
//
// ctx is carried so the opening counter can attach the active span as an
// exemplar, which is what makes an opening navigable from a dashboard back to
// the turn that caused it.
func (b *Breaker) RecordFailure(ctx context.Context, permit Permit) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == StateOpen || !b.honors(permit) {
		return
	}
	b.failures++
	if b.state == StateHalfOpen || b.failures >= b.failureThreshold {
		b.open(ctx)
	}
}

// RecordAbandoned releases a canceled half-open probe without counting a
// failure.
//
// A caller that gave up proved nothing about the dependency, so the failure
// count is left alone — but leaving its permit active would strand the breaker
// in half-open forever, refusing every later caller. Re-open from now instead,
// so the next caller gets a fresh, exclusive recovery probe.
func (b *Breaker) RecordAbandoned(ctx context.Context, permit Permit) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.honors(permit) || b.state != StateHalfOpen || !b.probeActive {
		return
	}
	b.open(ctx)
}

// open trips the breaker and restarts the cooldown. Callers hold b.mu.
func (b *Breaker) open(ctx context.Context) {
	b.onOpen(ctx, b.name)
	b.state = StateOpen
	b.openedAt = time.Now()
	b.probeActive = false
	b.generation++
}

// honors reports whether an outcome may be applied. Callers hold b.mu.
func (b *Breaker) honors(permit Permit) bool {
	return permit.admitted && permit.generation == b.generation
}

// breakerState is a consistent read of everything that describes a breaker at
// one instant. Taken under the lock so an observer cannot see a torn,
// half-applied transition.
type breakerState struct {
	openedAt    time.Time
	state       State
	failures    int
	generation  uint64
	probeActive bool
}

func (b *Breaker) snapshot() breakerState {
	b.mu.Lock()
	defer b.mu.Unlock()

	return breakerState{
		openedAt:    b.openedAt,
		state:       b.state,
		failures:    b.failures,
		generation:  b.generation,
		probeActive: b.probeActive,
	}
}

// BreakersConfig describes the breakers a runtime creates on demand.
type BreakersConfig struct {
	// OnOpen observes every opening of every breaker in the registry. See
	// [BreakerConfig].
	OnOpen func(ctx context.Context, name string)
	// FailureThreshold and ResetTimeout are handed to every breaker the registry
	// creates, exactly as the Python registry read them from the settings once.
	FailureThreshold int
	ResetTimeout     time.Duration
}

// Breakers is the per-resource breaker registry.
//
// It is a value a runtime owns rather than a package global, so two runtimes —
// and two tests — never share breaker state. That also removes the reset hook
// the Python module needed purely for test isolation.
//
// A nil *Breakers is the shipped default: AGENT_CIRCUIT_BREAKER_ENABLED is off,
// so no registry exists, which is a stronger statement than an empty one.
type Breakers struct {
	onOpen           func(ctx context.Context, name string)
	byName           map[string]*Breaker
	failureThreshold int
	resetTimeout     time.Duration
	mu               sync.Mutex
}

// NewBreakers builds the registry, validating the shared configuration once so
// that creating a breaker later cannot fail.
func NewBreakers(cfg BreakersConfig) (*Breakers, error) {
	if err := validateBreakerLimits(cfg.FailureThreshold, cfg.ResetTimeout); err != nil {
		return nil, err
	}
	return &Breakers{
		onOpen:           cfg.OnOpen,
		byName:           make(map[string]*Breaker),
		failureThreshold: cfg.FailureThreshold,
		resetTimeout:     cfg.ResetTimeout,
	}, nil
}

// Get returns the named breaker, creating it on first use.
//
// It is total rather than fallible because [NewBreakers] already validated
// everything a breaker needs: a tool name cannot make a valid threshold invalid.
func (r *Breakers) Get(name string) *Breaker {
	r.mu.Lock()
	defer r.mu.Unlock()

	breaker, ok := r.byName[name]
	if !ok {
		breaker = newBreaker(BreakerConfig{
			OnOpen:           r.onOpen,
			Name:             name,
			FailureThreshold: r.failureThreshold,
			ResetTimeout:     r.resetTimeout,
		})
		r.byName[name] = breaker
	}
	return breaker
}

// Names returns the resources that have a breaker, sorted.
//
// Sorted rather than in map order because Go randomizes map iteration, and an
// introspection surface that reports its contents in a different order on every
// call is useless for both a dashboard and a test.
func (r *Breakers) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Sorted(maps.Keys(r.byName))
}
