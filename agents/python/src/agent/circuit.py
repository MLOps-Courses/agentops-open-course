"""Circuit breaker — stop hammering a dependency that is already down (Chapter 4.5).

Bounded retries (``resilience.py``) give one flaky call a second chance. They do
the wrong thing when a dependency is *persistently* down: every turn pays the
full retry budget before failing, so a dead gateway turns into a pile of slow,
doomed calls. A circuit breaker is the complementary lever — after a run of
failures it *opens* and fails fast, sheds load off the struggling dependency,
then after a cooldown lets a single trial call test whether it recovered.

The breaker is deterministic and injectable (the clock is a constructor
argument) so the offline test gate exercises every transition without sleeping.
It is **opt-in**: ``AGENT_CIRCUIT_BREAKER_ENABLED`` defaults to ``false`` so the
default behavior — retry only — is unchanged until a builder turns it on. Like
every guarded write, it only ever wraps idempotent reads (``with_resilience``);
a breaker must never sit in front of a non-idempotent action.
"""

from __future__ import annotations

import time
from collections.abc import Callable
from dataclasses import dataclass, field
from enum import StrEnum

from opentelemetry import metrics

from .config import settings

# A counter (not a gauge) so Prometheus can alert on an opening *rate*: a breaker
# that flaps open repeatedly is a louder signal than one that is momentarily open.
_OPENED_COUNTER = metrics.get_meter("agentops.agent").create_counter(
    "agentops.circuit.opened_total",
    unit="1",
    description="Times a circuit breaker opened, by resource",
)


class CircuitState(StrEnum):
    """The three states of a standard circuit breaker."""

    CLOSED = "closed"  # calls flow; failures are counted
    OPEN = "open"  # calls fail fast until the cooldown elapses
    HALF_OPEN = "half_open"  # one trial call is allowed to test recovery


class CircuitOpenError(RuntimeError):
    """Raised instead of calling a dependency whose breaker is open."""


@dataclass(slots=True)
class CircuitBreaker:
    """A per-resource breaker with an injectable monotonic clock.

    ``allow`` is the gate the caller checks before doing work; ``record_success``
    and ``record_failure`` report the outcome so the breaker can advance its
    state. Keeping the clock injectable is what makes the transitions testable
    without real time passing.
    """

    name: str
    failure_threshold: int
    reset_timeout_s: float
    clock: Callable[[], float] = time.monotonic
    state: CircuitState = CircuitState.CLOSED
    failures: int = 0
    opened_at: float = field(default=0.0)

    def allow(self) -> bool:
        """Return whether a call may proceed, advancing OPEN → HALF_OPEN on cooldown.

        Closed and half-open both allow the call; open allows it only once the
        reset timeout has elapsed, at which point it moves to half-open to let a
        single trial through.
        """
        if self.state is CircuitState.OPEN and self.clock() - self.opened_at >= self.reset_timeout_s:
            self.state = CircuitState.HALF_OPEN
        return self.state is not CircuitState.OPEN

    def record_success(self) -> None:
        """A successful call closes the breaker and clears the failure count."""
        self.state = CircuitState.CLOSED
        self.failures = 0

    # --8<-- [start:record-failure]
    def record_failure(self) -> None:
        """A failed call trips the breaker once the threshold is reached.

        A failure while half-open re-opens immediately: the trial proved the
        dependency is still unhealthy, so start a fresh cooldown. A failure while
        already open is a no-op — the cooldown is already running and must not be
        extended indefinitely by failures that never actually reached the call.
        """
        self.failures += 1
        if self.state is CircuitState.OPEN:
            return
        if self.state is CircuitState.HALF_OPEN or self.failures >= self.failure_threshold:
            _OPENED_COUNTER.add(1, {"resource": self.name})
            self.state = CircuitState.OPEN
            self.opened_at = self.clock()

    # --8<-- [end:record-failure]


# One breaker per wrapped resource (tool name), created lazily from settings.
_BREAKERS: dict[str, CircuitBreaker] = {}


def get_breaker(name: str) -> CircuitBreaker:
    """Return the named breaker, creating it from the current settings once."""
    breaker = _BREAKERS.get(name)
    if breaker is None:
        breaker = CircuitBreaker(
            name=name,
            failure_threshold=settings.circuit_failure_threshold,
            reset_timeout_s=settings.circuit_reset_timeout_s,
        )
        _BREAKERS[name] = breaker
    return breaker


def reset_breakers() -> None:
    """Clear the breaker registry (test isolation between cases)."""
    _BREAKERS.clear()
