"""Unit tests for the persistent long-term memory tools (Ch. 3.4)."""

from types import SimpleNamespace
from typing import cast

from google.adk.tools.tool_context import ToolContext

from agent import longterm


def _context(user_id: str, session_id: str) -> ToolContext:
    return cast("ToolContext", SimpleNamespace(user_id=user_id, session=SimpleNamespace(id=session_id)))


def test_notes_persist_across_simulated_sessions() -> None:
    yesterday = _context("engineer", "session-mon")
    longterm.save_incident_note("INC-002", "Restarted inventory; crash-loop persists.", yesterday)
    today = _context("engineer", "session-tue")  # a brand-new conversation
    recalled = longterm.recall_incident_context("INC-002", today)
    assert recalled["count"] == 1
    assert "crash-loop persists" in recalled["notes"][0]["note"]


def test_recall_without_filter_returns_newest_first() -> None:
    context = _context("engineer", "session-1")
    longterm.save_incident_note("INC-001", "checked latency graphs", context)
    longterm.save_incident_note("INC-002", "escalated to fulfillment", context)
    recalled = longterm.recall_incident_context(tool_context=context)
    assert recalled["count"] == 2
    assert recalled["notes"][0]["incident_id"] == "INC-002"  # newest first


def test_memory_is_isolated_per_user() -> None:
    longterm.save_incident_note("INC-002", "private note from alice", _context("alice", "s1"))
    recalled = longterm.recall_incident_context("INC-002", _context("bob", "s2"))
    assert recalled["count"] == 0


def test_notes_are_redacted_before_persisting() -> None:
    context = _context("engineer", "s1")
    longterm.save_incident_note(
        "INC-002",
        "paged jane.doe@acme.com with api_key=super-secret-api-key-123456",
        context,
    )
    recalled = longterm.recall_incident_context("INC-002", context)
    assert "jane.doe@acme.com" not in str(recalled)
    assert "super-secret-api-key-123456" not in str(recalled)
    assert "<EMAIL_ADDRESS>" in str(recalled)
    assert "api_key=<SECRET>" in str(recalled)
    assert recalled["count"] == 1


def test_invalid_inputs_are_rejected() -> None:
    context = _context("engineer", "s1")
    assert "error" in longterm.save_incident_note("ticket-9", "note", context)
    assert "orphaned memory" in longterm.save_incident_note("INC-999", "note", context)["error"]
    assert "error" in longterm.save_incident_note("INC-002", "   ", context)
    assert "error" in longterm.save_incident_note("INC-002", "x" * 2001, context)
    assert "error" in longterm.recall_incident_context("not-an-id", context)


def test_direct_calls_use_a_stable_anonymous_identity() -> None:
    longterm.save_incident_note("INC-001", "saved without a session")
    recalled = longterm.recall_incident_context("INC-001")
    assert recalled["count"] == 1


def test_memory_lives_in_the_disposable_state_dir() -> None:
    from agent.config import settings

    path = longterm.memory_db_path()
    assert path == str(settings.state_dir / "memory.db")  # disposable state, never seed data


def test_forget_user_memory_erases_only_that_user() -> None:
    longterm.save_incident_note("INC-002", "alice private note", _context("alice", "s1"))
    longterm.save_incident_note("INC-001", "bob private note", _context("bob", "s2"))
    result = longterm.forget_user_memory("alice")
    assert result["forgotten"] == {"user_id": "alice", "count": 1}
    assert longterm.recall_incident_context(tool_context=_context("alice", "s3"))["count"] == 0
    assert longterm.recall_incident_context(tool_context=_context("bob", "s4"))["count"] == 1  # bob is untouched


def test_forget_user_memory_rejects_empty_user() -> None:
    assert "error" in longterm.forget_user_memory("   ")


def test_forget_is_not_an_agent_tool() -> None:
    # Erasure is an operator action; the model must never be handed the capability.
    assert longterm.forget_user_memory not in longterm.MEMORY_TOOLS
