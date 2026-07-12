"""Unit tests for tool deadlines, bounded retries, and the no-retry-on-write rule."""

import asyncio
import inspect
import time
from typing import Any

import pytest

from agent import actions, resilience
from agent.resilience import ToolDeadlineError, with_resilience


def test_wrapper_preserves_tool_schema_inputs() -> None:
    def sample_tool(query: str = "") -> dict[str, Any]:
        """Docstring the model sees."""
        return {"query": query}

    wrapped = with_resilience(sample_tool)
    assert getattr(wrapped, "__name__", None) == "sample_tool"
    assert wrapped.__doc__ == "Docstring the model sees."
    assert list(inspect.signature(wrapped).parameters) == ["query"]


def test_transient_failure_recovers(monkeypatch) -> None:
    monkeypatch.setattr(resilience.settings, "max_retries", 2)
    monkeypatch.setattr(resilience.settings, "retry_backoff_s", 0.001)
    calls = {"count": 0}

    def flaky() -> dict[str, Any]:
        calls["count"] += 1
        if calls["count"] == 1:
            raise ConnectionError("transient hiccup")
        return {"ok": True}

    assert asyncio.run(with_resilience(flaky)()) == {"ok": True}
    assert calls["count"] == 2


def test_permanent_failure_surfaces_with_context(monkeypatch) -> None:
    monkeypatch.setattr(resilience.settings, "max_retries", 1)
    monkeypatch.setattr(resilience.settings, "retry_backoff_s", 0.001)

    def broken() -> dict[str, Any]:
        raise ConnectionError("gateway is down")

    with pytest.raises(RuntimeError, match="failed after 2 attempts") as excinfo:
        asyncio.run(with_resilience(broken)())
    assert isinstance(excinfo.value.__cause__, ConnectionError)


def test_backoff_grows_exponentially(monkeypatch) -> None:
    monkeypatch.setattr(resilience.settings, "max_retries", 2)
    monkeypatch.setattr(resilience.settings, "retry_backoff_s", 0.5)
    delays: list[float] = []

    async def fake_sleep(delay: float) -> None:
        delays.append(delay)

    monkeypatch.setattr(resilience.asyncio, "sleep", fake_sleep)

    def broken() -> dict[str, Any]:
        raise ConnectionError("still down")

    with pytest.raises(RuntimeError, match="failed after 3 attempts"):
        asyncio.run(with_resilience(broken)())
    assert delays == [0.5, 1.0]


def test_deadline_raises_without_retry(monkeypatch) -> None:
    monkeypatch.setattr(resilience.settings, "tool_timeout_s", 0.05)
    monkeypatch.setattr(resilience.settings, "max_retries", 3)
    calls = {"count": 0}

    def hanging() -> dict[str, Any]:
        calls["count"] += 1
        time.sleep(1)
        return {}

    with pytest.raises(ToolDeadlineError, match="AGENT_TOOL_TIMEOUT_S"):
        asyncio.run(with_resilience(hanging)())
    assert calls["count"] == 1  # a deadline is a budget: never retried


def test_guarded_actions_are_never_wrapped() -> None:
    """Retrying a non-idempotent write could apply it twice: actions stay raw."""
    wrapped_functions = {tool.func for tool in actions.ACTION_TOOLS}
    assert wrapped_functions == {actions.restart_service, actions.resolve_incident}
    for tool in actions.ACTION_TOOLS:
        assert not inspect.iscoroutinefunction(tool.func)
