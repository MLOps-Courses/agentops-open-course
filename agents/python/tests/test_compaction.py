"""Unit tests for deterministic context compaction (bounded history)."""

from typing import cast

from google.adk.agents.callback_context import CallbackContext
from google.adk.models.llm_request import LlmRequest
from google.genai import types

from agent import compaction


def _text(role: str, text: str) -> types.Content:
    return types.Content(role=role, parts=[types.Part(text=text)])


def _call(name: str) -> types.Content:
    return types.Content(role="model", parts=[types.Part(function_call=types.FunctionCall(name=name, args={}))])


def _result(name: str) -> types.Content:
    return types.Content(
        role="user",
        parts=[types.Part(function_response=types.FunctionResponse(name=name, response={"ok": True}))],
    )


def _compact(contents: list[types.Content]) -> LlmRequest:
    request = LlmRequest(contents=list(contents))
    result = compaction.compact_history(cast("CallbackContext", None), request)
    assert result is None  # compaction never short-circuits the model call
    return request


def test_disabled_by_default(monkeypatch) -> None:
    monkeypatch.setattr(compaction.settings, "max_history_messages", None)
    history = [_text("user", f"turn {i}") for i in range(10)]
    request = _compact(history)
    assert request.contents == history  # untouched when the knob is unset


def test_noop_when_within_budget(monkeypatch) -> None:
    monkeypatch.setattr(compaction.settings, "max_history_messages", 5)
    history = [_text("user", f"turn {i}") for i in range(5)]
    request = _compact(history)
    assert request.contents == history


def test_compacts_and_preserves_recent_messages(monkeypatch) -> None:
    monkeypatch.setattr(compaction.settings, "max_history_messages", 3)
    history = [_text("user", f"turn {i}") for i in range(8)]
    request = _compact(history)
    # One synthetic marker plus the three most recent messages, in order.
    assert len(request.contents) == 4
    assert request.contents[1:] == history[-3:]
    marker = request.contents[0]
    assert marker.role == "user"
    assert marker.parts is not None
    assert marker.parts[0].text is not None
    assert marker.parts[0].text.startswith("[history compacted: 5 earlier message(s)")


def test_marker_lists_elided_tool_names(monkeypatch) -> None:
    monkeypatch.setattr(compaction.settings, "max_history_messages", 2)
    history = [
        _text("user", "diagnose INC-001"),
        _call("get_incident"),
        _result("get_incident"),
        _call("search_service_logs"),
        _result("search_service_logs"),
        _text("user", "what next?"),
    ]
    request = _compact(history)
    assert request.contents[0].parts is not None
    note = request.contents[0].parts[0].text
    assert note is not None
    assert "get_incident" in note
    assert "search_service_logs" in note


def test_window_never_opens_on_orphan_tool_result(monkeypatch) -> None:
    monkeypatch.setattr(compaction.settings, "max_history_messages", 3)
    # With keep=3 the raw cut lands on the tool result at index 3, whose matching
    # call at index 1 would be dropped; compaction must advance past it.
    history = [
        _text("user", "diagnose INC-001"),
        _call("get_incident"),
        _text("model", "looking"),
        _result("get_incident"),  # index 3 — a bare tool result at the raw boundary
        _text("model", "here is the incident"),
        _text("user", "what next?"),
    ]
    request = _compact(history)
    first_kept = request.contents[1]
    assert first_kept.parts is not None
    assert first_kept.parts[0].function_response is None  # window opens on a real message
    # The orphaned result was folded into the elided span (4 messages, not 3).
    assert request.contents[0].parts is not None
    assert request.contents[0].parts[0].text is not None
    assert request.contents[0].parts[0].text.startswith("[history compacted: 4 earlier message(s)")
