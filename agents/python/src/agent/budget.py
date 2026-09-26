"""Token budgets and per-session cost attribution (Chapter 7.3).

Every production deployment must answer two questions: "how many tokens did
this session consume?" and "what happens when a session exceeds its budget?".
``record_token_usage`` accumulates usage from each model response into session
state (persisted across turns by the session service) and emits it as OTel
span attributes and metrics. ``enforce_token_budget`` refuses the next model
call once tracked usage reaches ``AGENT_MAX_TOKENS_PER_SESSION``. The last
admitted call can cross the threshold, and responses without usage metadata
cannot be counted. Costs are tokens times configurable per-1k prices — 0 by
default means prices are unconfigured, not that the default hosted model is
free. No vendor pricing is hardcoded.
"""

from __future__ import annotations

from collections.abc import Iterator
from contextlib import contextmanager
from threading import Lock

from google.adk.agents.callback_context import CallbackContext
from google.adk.models.llm_request import LlmRequest
from google.adk.models.llm_response import LlmResponse
from google.genai import types
from opentelemetry import metrics, trace

from .config import settings

# Session-state keys. No ``temp:`` prefix, so DatabaseSessionService persists
# the running totals across turns — the budget covers the whole conversation.
INPUT_TOKENS_KEY = "budget:input_tokens"
OUTPUT_TOKENS_KEY = "budget:output_tokens"

# ADK exposes the logical session identity on CallbackContext. Reference
# counting keeps one process-local lock alive while callbacks are using or
# waiting for it, then removes the registry entry after the last waiter. This
# threading lock protects synchronous callbacks; server._SessionSerializingRunner
# uses a separate asyncio lock around whole A2A invocations.
type _SessionKey = tuple[str, str, str]
_SESSION_LOCKS_GUARD = Lock()
_SESSION_LOCKS: dict[_SessionKey, Lock] = {}
_SESSION_LOCK_REFS: dict[_SessionKey, int] = {}

# A counter (vs span attributes) so Prometheus can graph token throughput.
_TOKEN_COUNTER = metrics.get_meter("agentops.agent").create_counter(
    "agentops.tokens",
    unit="token",
    description="Model tokens consumed by the AgentOps Agent, by direction",
)


def _session_key(callback_context: CallbackContext) -> _SessionKey:
    """Return ADK's stable logical session identity."""
    session = callback_context.session
    return session.app_name, session.user_id, session.id


@contextmanager
def _session_usage_lock(callback_context: CallbackContext) -> Iterator[None]:
    """Serialize budget-state access for one session in this process."""
    key = _session_key(callback_context)
    with _SESSION_LOCKS_GUARD:
        lock = _SESSION_LOCKS.get(key)
        if lock is None:
            lock = Lock()
            _SESSION_LOCKS[key] = lock
        _SESSION_LOCK_REFS[key] = _SESSION_LOCK_REFS.get(key, 0) + 1

    try:
        with lock:
            yield
    finally:
        with _SESSION_LOCKS_GUARD:
            remaining = _SESSION_LOCK_REFS[key] - 1
            if remaining == 0:
                _SESSION_LOCK_REFS.pop(key)
                _SESSION_LOCKS.pop(key)
            else:
                _SESSION_LOCK_REFS[key] = remaining


def estimate_cost(input_tokens: int, output_tokens: int) -> float:
    """Estimate spend from per-1k prices; zero prices mean unconfigured attribution."""
    return (input_tokens / 1000) * settings.input_price_per_1k + (output_tokens / 1000) * settings.output_price_per_1k


def session_usage(callback_context: CallbackContext) -> tuple[int, int]:
    """Return the session's accumulated (input, output) token counts."""
    return (
        int(callback_context.state.get(INPUT_TOKENS_KEY, 0)),
        int(callback_context.state.get(OUTPUT_TOKENS_KEY, 0)),
    )


def enforce_token_budget(callback_context: CallbackContext, llm_request: LlmRequest) -> LlmResponse | None:
    """``before_model_callback``: refuse work past the per-session token budget.

    Returning a response here short-circuits the model call, so the engineer
    gets an actionable message instead of a silent failure or an open-ended bill.
    """
    del llm_request
    if settings.max_tokens_per_session is None:
        return None
    # --8<-- [start:record-session-usage]
    with _session_usage_lock(callback_context):
        input_tokens, output_tokens = session_usage(callback_context)
    used = input_tokens + output_tokens
    if used < settings.max_tokens_per_session:
        return None
    message = (
        f"This session has exhausted its token budget ({used} of {settings.max_tokens_per_session} tokens used). "
        "Start a new session to continue, or raise AGENT_MAX_TOKENS_PER_SESSION if the work warrants it."
    )
    return LlmResponse(
        content=types.Content(role="model", parts=[types.Part(text=message)]),
        error_code="TOKEN_BUDGET_EXHAUSTED",
        error_message="Per-session token budget exhausted.",
    )


def record_token_usage(callback_context: CallbackContext, llm_response: LlmResponse) -> None:
    """``after_model_callback``: attribute this turn's tokens to the session.

    Accumulates into session state and emits OTel span attributes (visible in
    MLflow traces) plus a metric counter (scraped by Prometheus). Returns
    ``None`` so the response continues to the next callback unchanged.
    """
    usage = llm_response.usage_metadata
    if usage is None:
        return
    turn_input = (usage.prompt_token_count or 0) + (usage.tool_use_prompt_token_count or 0)
    turn_output = (usage.candidates_token_count or 0) + (usage.thoughts_token_count or 0)
    classified_total = turn_input + turn_output
    # Some compatible providers report only a total. Keep the budget fail-closed
    # by assigning otherwise-unclassified usage to the output bucket rather than
    # silently recording zero tokens.
    turn_output += max((usage.total_token_count or classified_total) - classified_total, 0)
    with _session_usage_lock(callback_context):
        input_tokens, output_tokens = session_usage(callback_context)
        input_tokens += turn_input
        output_tokens += turn_output
        callback_context.state[INPUT_TOKENS_KEY] = input_tokens
        callback_context.state[OUTPUT_TOKENS_KEY] = output_tokens

    _TOKEN_COUNTER.add(turn_input, {"direction": "input"})
    _TOKEN_COUNTER.add(turn_output, {"direction": "output"})
    span = trace.get_current_span()
    span.set_attribute("agentops.tokens.session.input", input_tokens)
    span.set_attribute("agentops.tokens.session.output", output_tokens)
    span.set_attribute("agentops.tokens.session.total", input_tokens + output_tokens)
    span.set_attribute("agentops.cost.session.estimate", estimate_cost(input_tokens, output_tokens))
    # --8<-- [end:record-session-usage]
