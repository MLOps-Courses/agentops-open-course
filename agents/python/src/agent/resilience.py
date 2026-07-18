"""Resilience patterns — bounded retries, backoff, and deadlines (Chapter 4.5).

Transient failures (an Ollama cold start, a gateway restart, a locked SQLite
file) should cost a retry, not a failed turn — and a slow call should hit a
deadline, not hang forever. Three seams get that treatment:

- Model calls: native Gemini uses the SDK's ``HttpRetryOptions``; direct Ollama
  and agentgateway pass ``timeout``/``max_retries`` to the same OpenAI-compatible
  SDK path (see ``model.py``).
- MCP calls: connection parameters carry explicit timeouts (``mcp_client.py``).
- Read tools: ``with_resilience`` wraps each idempotent read in a deadline plus
  exponential backoff. Guarded write actions (``restart_service``,
  ``resolve_incident``) are deliberately **never** wrapped: retrying a
  non-idempotent action can apply it twice.
"""

from __future__ import annotations

import asyncio
import functools
import logging
from collections.abc import Callable
from typing import Any

from .circuit import CircuitOpenError, get_breaker
from .config import settings

logger = logging.getLogger(__name__)


class ToolDeadlineError(TimeoutError):
    """The caller stopped waiting after ``AGENT_TOOL_TIMEOUT_S`` elapsed."""


def with_resilience(func: Callable[..., dict[str, Any]]) -> Callable[..., Any]:
    """Wrap an idempotent read tool with a deadline and bounded retries.

    The sync function runs in a worker thread so ``asyncio.wait_for`` can stop
    the agent turn from waiting after the deadline (ADK calls sync tools inline
    on the event loop, where a timeout could never fire). Python cannot cancel a
    thread already executing the function, so that idempotent read may finish in
    the background; this wrapper must never be applied to writes.
    ``functools.wraps`` preserves the signature and docstring ADK reads to build
    the tool schema.
    """

    tool_name = getattr(func, "__name__", repr(func))

    @functools.wraps(func)
    async def wrapper(**kwargs: Any) -> dict[str, Any]:
        # When enabled, an open breaker short-circuits the whole retry loop: a
        # dependency that just failed its budget should not be hammered again.
        breaker = get_breaker(tool_name) if settings.circuit_breaker_enabled else None
        if breaker is not None and not breaker.allow():
            logger.warning("Tool %s circuit is open; failing fast without a call", tool_name)
            raise CircuitOpenError(
                f"Tool {tool_name!r} circuit is open after repeated failures; "
                f"retrying in at most {settings.circuit_reset_timeout_s:.0f}s (AGENT_CIRCUIT_RESET_TIMEOUT_S)."
            )
        last_error: Exception | None = None
        for attempt in range(settings.max_retries + 1):
            try:
                result = await asyncio.wait_for(
                    asyncio.to_thread(func, **kwargs),
                    timeout=settings.tool_timeout_s,
                )
            except TimeoutError:
                # A deadline is a budget, not a transient blip: do not retry,
                # the next attempt would most likely burn the same budget.
                logger.error("Tool %s exceeded its %.1fs deadline", tool_name, settings.tool_timeout_s)
                if breaker is not None:
                    breaker.record_failure()
                raise ToolDeadlineError(
                    f"Tool {tool_name!r} exceeded its {settings.tool_timeout_s:.0f}s deadline (AGENT_TOOL_TIMEOUT_S)."
                ) from None
            except Exception as error:  # retry boundary: transient faults deserve a second chance
                last_error = error
                if attempt < settings.max_retries:
                    delay = settings.retry_backoff_s * (2**attempt)
                    logger.warning(
                        "Tool %s failed (attempt %d/%d), retrying in %.1fs: %s",
                        tool_name,
                        attempt + 1,
                        settings.max_retries + 1,
                        delay,
                        error,
                    )
                    await asyncio.sleep(delay)
            else:
                if breaker is not None:
                    breaker.record_success()
                return result
        # Exhausted retries: count one breaker failure, then surface the root cause.
        if breaker is not None:
            breaker.record_failure()
        raise RuntimeError(
            f"Tool {tool_name!r} failed after {settings.max_retries + 1} attempts (AGENT_MAX_RETRIES)."
        ) from last_error

    return wrapper
