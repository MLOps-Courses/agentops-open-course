"""Unit tests for the persistent long-term memory tools (Ch. 3.4)."""

import inspect
import sqlite3
from contextlib import closing
from types import SimpleNamespace
from typing import cast

import pytest
from google.adk.tools.tool_context import ToolContext

from agent import data, longterm
from tests.domain import REFERENCE_DOMAIN

_CHECKOUT_INCIDENT = REFERENCE_DOMAIN.incidents.checkout_latency
_INVENTORY = REFERENCE_DOMAIN.services.inventory
_INVENTORY_INCIDENT = REFERENCE_DOMAIN.incidents.inventory_down


def test_recall_tool_uses_the_shared_read_resilience_boundary() -> None:
    assert longterm.MEMORY_TOOLS[0] is longterm.RECALL_INCIDENT_CONTEXT_TOOL
    assert inspect.iscoroutinefunction(longterm.RECALL_INCIDENT_CONTEXT_TOOL)
    assert inspect.unwrap(longterm.RECALL_INCIDENT_CONTEXT_TOOL) is longterm.recall_incident_context


def _context(user_id: str, session_id: str) -> ToolContext:
    return cast("ToolContext", SimpleNamespace(user_id=user_id, session=SimpleNamespace(id=session_id)))


def test_notes_persist_across_simulated_sessions() -> None:
    yesterday = _context("engineer", "session-mon")
    longterm.save_incident_note(
        _INVENTORY_INCIDENT,
        f"Restarted {_INVENTORY}; crash-loop persists.",
        yesterday,
    )
    today = _context("engineer", "session-tue")  # a brand-new conversation
    recalled = longterm.recall_incident_context(_INVENTORY_INCIDENT, today)
    assert recalled["count"] == 1
    assert "crash-loop persists" in recalled["notes"][0]["note"]


def test_recall_without_filter_returns_newest_first() -> None:
    context = _context("engineer", "session-1")
    longterm.save_incident_note(_CHECKOUT_INCIDENT, "checked latency graphs", context)
    longterm.save_incident_note(_INVENTORY_INCIDENT, "escalated to fulfillment", context)
    recalled = longterm.recall_incident_context(tool_context=context)
    assert recalled["count"] == 2
    assert recalled["notes"][0]["incident_id"] == _INVENTORY_INCIDENT  # newest first


def test_memory_is_isolated_per_user() -> None:
    longterm.save_incident_note(_INVENTORY_INCIDENT, "private note from alice", _context("alice", "s1"))
    recalled = longterm.recall_incident_context(_INVENTORY_INCIDENT, _context("bob", "s2"))
    assert recalled["count"] == 0


def test_recall_before_first_note_does_not_create_state(monkeypatch) -> None:
    monkeypatch.setattr(longterm.settings, "writes_disabled", True)
    assert not longterm.settings.state_dir.exists()
    assert longterm.recall_incident_context() == {"count": 0, "notes": []}
    assert not longterm.settings.state_dir.exists()


def test_recall_uses_read_only_connection_and_preserves_existing_store(monkeypatch) -> None:
    longterm.save_incident_note(_INVENTORY_INCIDENT, "restart failed")
    path = longterm.settings.state_dir / "memory.db"
    before = path.read_bytes()
    connect = sqlite3.connect
    connections = []

    def read_only_connect(database, **kwargs):
        connection = connect(database, **kwargs)
        connections.append(connection)
        # mode=ro must prevent mutations even before application query_only setup.
        with pytest.raises(sqlite3.OperationalError, match="readonly"):
            connection.execute("DELETE FROM incident_notes")
        return connection

    monkeypatch.setattr(longterm.sqlite3, "connect", read_only_connect)
    monkeypatch.setattr(longterm.settings, "writes_disabled", True)
    assert longterm.recall_incident_context()["count"] == 1
    assert len(connections) == 1
    assert path.read_bytes() == before


@pytest.mark.parametrize("store", ["corrupt", "missing-schema", "directory"])
def test_recall_rejects_invalid_existing_store_without_repairing_it(store) -> None:
    longterm.settings.state_dir.mkdir()
    path = longterm.settings.state_dir / "memory.db"
    if store == "corrupt":
        path.write_bytes(b"not a SQLite database")
    elif store == "missing-schema":
        with closing(sqlite3.connect(path)) as connection:
            connection.execute("CREATE TABLE unrelated (value TEXT)")
            connection.commit()
    else:
        path.mkdir()
    before = path.read_bytes() if path.is_file() else None
    with pytest.raises(data.DataAccessError, match=r"long-term memory|Long-term memory"):
        longterm.recall_incident_context()
    if before is not None:
        assert path.read_bytes() == before
    else:
        assert path.is_dir()


def test_notes_are_redacted_before_persisting() -> None:
    context = _context("engineer", "s1")
    longterm.save_incident_note(
        _INVENTORY_INCIDENT,
        "paged jane.doe@acme.com with api_key=super-secret-api-key-123456",
        context,
    )
    recalled = longterm.recall_incident_context(_INVENTORY_INCIDENT, context)
    assert "jane.doe@acme.com" not in str(recalled)
    assert "super-secret-api-key-123456" not in str(recalled)
    assert "<EMAIL_ADDRESS>" in str(recalled)
    assert "api_key=<SECRET>" in str(recalled)
    assert recalled["count"] == 1


def test_kill_switch_refuses_note_before_state_write(monkeypatch) -> None:
    monkeypatch.setattr(longterm.settings, "writes_disabled", True)
    result = longterm.save_incident_note(
        _INVENTORY_INCIDENT,
        f"Restarted {_INVENTORY}.",
        _context("engineer", "s1"),
    )
    assert "AGENT_WRITES_DISABLED" in result["error"]
    assert not (longterm.settings.state_dir / "memory.db").exists()


def test_invalid_inputs_are_rejected() -> None:
    context = _context("engineer", "s1")
    assert "error" in longterm.save_incident_note("ticket-9", "note", context)
    assert "orphaned memory" in longterm.save_incident_note("INC-999", "note", context)["error"]
    assert "error" in longterm.save_incident_note(_INVENTORY_INCIDENT, "   ", context)
    assert "error" in longterm.save_incident_note(_INVENTORY_INCIDENT, "x" * 2001, context)
    assert "error" in longterm.recall_incident_context("not-an-id", context)


def test_direct_calls_use_a_stable_anonymous_identity() -> None:
    longterm.save_incident_note(_CHECKOUT_INCIDENT, "saved without a session")
    recalled = longterm.recall_incident_context(_CHECKOUT_INCIDENT)
    assert recalled["count"] == 1


def test_memory_lives_in_the_disposable_state_dir() -> None:
    from agent.config import settings

    path = longterm.memory_db_path()
    assert path == str(settings.state_dir / "memory.db")  # disposable state, never seed data


def test_forget_user_memory_erases_only_that_user() -> None:
    longterm.save_incident_note(_INVENTORY_INCIDENT, "alice private note", _context("alice", "s1"))
    longterm.save_incident_note(_CHECKOUT_INCIDENT, "bob private note", _context("bob", "s2"))
    result = longterm.forget_user_memory("alice")
    assert result["forgotten"] == {"user_id": "alice", "count": 1}
    assert longterm.recall_incident_context(tool_context=_context("alice", "s3"))["count"] == 0
    assert longterm.recall_incident_context(tool_context=_context("bob", "s4"))["count"] == 1  # bob is untouched


def test_forget_user_memory_rejects_empty_user() -> None:
    assert "error" in longterm.forget_user_memory("   ")


def test_forget_is_not_an_agent_tool() -> None:
    # Erasure is an operator action; the model must never be handed the capability.
    assert longterm.forget_user_memory not in longterm.MEMORY_TOOLS
