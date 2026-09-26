"""Bounded conversation history — deterministic context compaction (Chapter 3.4).

An incident investigation can run long. Each user turn may expand into a model
tool call, a tool response, and a model answer, and ADK's session service keeps
every message forever. Left unbounded, the prompt grows every turn until it
strains the model's context window, inflates token cost, and slows each call —
the same failure the token budget (Chapter 7.3) bounds from the cost side, seen
from the context side.

``compact_history`` is a ``before_model_callback`` that keeps the most recent
messages and replaces the older ones with a single synthetic note recording how
many were elided and which tools they touched. It is deterministic and
model-free — no summarization model call — so the offline test gate stays exact,
and it is off by default: set ``AGENT_MAX_HISTORY_MESSAGES`` to enable it.
Summarizing the elided span with the model is the natural extension, traded
against determinism and one extra model call per compaction.

The rewrite is ephemeral: ADK rebuilds ``llm_request.contents`` from the stored
session events every turn, so nothing is deleted from the session and no marker
ever accumulates — each call compacts the full history afresh.
"""

from __future__ import annotations

from google.adk.agents.callback_context import CallbackContext
from google.adk.models.llm_request import LlmRequest
from google.adk.models.llm_response import LlmResponse
from google.genai import types

from .config import settings

# A trusted, first-party note (never model- or tool-authored), so it carries no
# injection risk. ``user`` role keeps it in the conversational frame the model reads.
_MARKER_ROLE = "user"


def _has_function_response(content: types.Content) -> bool:
    """True if the message carries a tool result (a ``function_response`` part)."""
    return any(part.function_response is not None for part in content.parts or ())


def _earlier_tool_names(contents: list[types.Content]) -> list[str]:
    """Distinct tool names called in the elided span, in first-seen order."""
    names: list[str] = []
    for content in contents:
        for part in content.parts or ():
            call = part.function_call
            if call is not None and call.name and call.name not in names:
                names.append(call.name)
    return names


def _marker(elided: list[types.Content]) -> types.Content:
    """A single message standing in for the elided span."""
    note = f"[history compacted: {len(elided)} earlier message(s) elided to bound context"
    tool_names = _earlier_tool_names(elided)
    if tool_names:
        note += f"; tools used earlier: {', '.join(tool_names)}"
    note += "]"
    return types.Content(role=_MARKER_ROLE, parts=[types.Part(text=note)])


# --8<-- [start:compact-history]
def compact_history(callback_context: CallbackContext, llm_request: LlmRequest) -> LlmResponse | None:
    """``before_model_callback``: keep only the most recent messages in the prompt.

    Returns ``None`` (never short-circuits the model call); it only rewrites
    ``llm_request.contents`` in place. Disabled unless ``AGENT_MAX_HISTORY_MESSAGES``
    is set, and a no-op until the history is longer than that budget.
    """
    del callback_context  # compaction depends only on the outgoing request
    keep = settings.max_history_messages
    if keep is None:
        return None
    contents = llm_request.contents
    if len(contents) <= keep:
        return None
    cut = len(contents) - keep
    # A tool batch is indivisible: retain its call and every response, including
    # when the latest message is still a tool result. Advancing the cut either
    # loses fresh evidence or leaves an orphan result that providers reject.
    # The message target is therefore soft at this protocol boundary.
    if _has_function_response(contents[cut]):
        while cut > 0:
            cut -= 1
            if any(part.function_call is not None for part in contents[cut].parts or ()):
                break
    if cut == 0:
        return None
    llm_request.contents[:] = [_marker(contents[:cut]), *contents[cut:]]
    return None


# --8<-- [end:compact-history]
