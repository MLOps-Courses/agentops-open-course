"""Unit tests for the circuit breaker and its wiring into ``with_resilience``.

The breaker's clock is injectable, so every open/half-open/close transition is
exercised deterministically with a fake monotonic clock — no real time passes.
"""

import asyncio
from typing import Any

import pytest

from agent import circuit, resilience
from agent.circuit import CircuitBreaker, CircuitOpenError, CircuitState, get_breaker, reset_breakers
from agent.resilience import with_resilience


class _FakeClock:
    """A monotonic clock the test advances explicitly."""

    def __init__(self) -> None:
        self.now = 0.0

    def __call__(self) -> float:
        return self.now


@pytest.fixture(autouse=True)
def _isolate_breakers() -> Any:
    """Each test starts with an empty breaker registry."""
    reset_breakers()
    yield
    reset_breakers()


def test_opens_after_consecutive_failures() -> None:
    breaker = CircuitBreaker(name="reads", failure_threshold=3, reset_timeout_s=30.0, clock=_FakeClock())
    for _ in range(2):
        breaker.record_failure()
    assert breaker.state is CircuitState.CLOSED
    assert breaker.allow() is True
    breaker.record_failure()  # third failure trips it
    assert breaker.state is CircuitState.OPEN
    assert breaker.allow() is False  # fails fast while open


def test_half_open_after_cooldown_then_closes_on_success() -> None:
    clock = _FakeClock()
    breaker = CircuitBreaker(name="reads", failure_threshold=1, reset_timeout_s=30.0, clock=clock)
    breaker.record_failure()
    assert breaker.allow() is False
    clock.now = 29.0
    assert breaker.allow() is False  # cooldown not elapsed
    clock.now = 30.0
    assert breaker.allow() is True  # one trial call permitted
    assert breaker.state is CircuitState.HALF_OPEN
    breaker.record_success()
    assert breaker.state is CircuitState.CLOSED
    assert breaker.failures == 0


def test_repeated_failure_while_open_keeps_one_opened_transition() -> None:
    clock = _FakeClock()
    breaker = CircuitBreaker(name="reads", failure_threshold=1, reset_timeout_s=30.0, clock=clock)
    breaker.record_failure()  # opens
    assert breaker.state is CircuitState.OPEN
    opened_at = breaker.opened_at
    clock.now = 5.0
    breaker.record_failure()  # already open: does not re-stamp the cooldown
    assert breaker.state is CircuitState.OPEN
    assert breaker.opened_at == opened_at


def test_half_open_failure_reopens_with_fresh_cooldown() -> None:
    clock = _FakeClock()
    breaker = CircuitBreaker(name="reads", failure_threshold=1, reset_timeout_s=10.0, clock=clock)
    breaker.record_failure()
    clock.now = 10.0
    assert breaker.allow() is True  # half-open trial
    clock.now = 12.0
    breaker.record_failure()  # trial failed: reopen from now
    assert breaker.state is CircuitState.OPEN
    assert breaker.opened_at == 12.0
    assert breaker.allow() is False


def test_with_resilience_fails_fast_when_circuit_open(monkeypatch) -> None:
    monkeypatch.setattr(resilience.settings, "circuit_breaker_enabled", True)
    monkeypatch.setattr(resilience.settings, "circuit_failure_threshold", 1)
    monkeypatch.setattr(resilience.settings, "circuit_reset_timeout_s", 30.0)
    monkeypatch.setattr(resilience.settings, "max_retries", 0)
    calls = {"count": 0}

    def broken() -> dict[str, Any]:
        calls["count"] += 1
        raise ConnectionError("dependency down")

    wrapped = with_resilience(broken)
    with pytest.raises(RuntimeError, match="failed after 1 attempt"):
        asyncio.run(wrapped())  # first failure opens the breaker
    assert get_breaker("broken").state is CircuitState.OPEN
    with pytest.raises(CircuitOpenError, match="circuit is open"):
        asyncio.run(wrapped())  # second call is refused without invoking the function
    assert calls["count"] == 1  # the open circuit shed the second call


def test_success_keeps_circuit_closed(monkeypatch) -> None:
    monkeypatch.setattr(resilience.settings, "circuit_breaker_enabled", True)
    monkeypatch.setattr(resilience.settings, "circuit_failure_threshold", 2)
    monkeypatch.setattr(resilience.settings, "max_retries", 0)

    def healthy() -> dict[str, Any]:
        return {"ok": True}

    assert asyncio.run(with_resilience(healthy)()) == {"ok": True}
    assert get_breaker("healthy").state is CircuitState.CLOSED


def test_deadline_counts_as_a_breaker_failure(monkeypatch) -> None:
    import time

    monkeypatch.setattr(resilience.settings, "circuit_breaker_enabled", True)
    monkeypatch.setattr(resilience.settings, "circuit_failure_threshold", 1)
    monkeypatch.setattr(resilience.settings, "tool_timeout_s", 0.05)
    monkeypatch.setattr(resilience.settings, "max_retries", 3)

    def hanging() -> dict[str, Any]:
        time.sleep(1)
        return {}

    with pytest.raises(resilience.ToolDeadlineError):
        asyncio.run(with_resilience(hanging)())
    assert get_breaker("hanging").state is CircuitState.OPEN  # a persistent timeout trips the breaker


def test_disabled_circuit_leaves_retry_only_behavior(monkeypatch) -> None:
    monkeypatch.setattr(resilience.settings, "circuit_breaker_enabled", False)
    monkeypatch.setattr(resilience.settings, "max_retries", 0)
    calls = {"count": 0}

    def broken() -> dict[str, Any]:
        calls["count"] += 1
        raise ConnectionError("dependency down")

    wrapped = with_resilience(broken)
    for _ in range(3):
        with pytest.raises(RuntimeError, match="failed after 1 attempt"):
            asyncio.run(wrapped())
    assert calls["count"] == 3  # no breaker: every call still reaches the function
    assert circuit._BREAKERS == {}  # noqa: SLF001 — asserts no breaker was created when disabled
