"""Persistent long-term memory — incident context across conversations (Chapter 3.4).

In-session history dies with the session and runbook retrieval only knows the
knowledge base. This store remembers what an engineer was doing *across*
conversations: the incident under investigation, remediation already attempted,
outcome notes. Memory is exposed as explicit tools (``recall_incident_context``,
``save_incident_note``) rather than silent context stuffing, so every read and
write is visible in the trace, auditable, and testable. Notes live in the
disposable state directory (never the seed dataset), are isolated by the
runtime-supplied user id, and pass PII redaction before persisting — memory is a
data store like any other. Cross-session recall therefore requires a stable user
id; the unauthenticated A2A adapter's context-bound synthetic id is intentionally
not human identity. ``mise run data:reset`` clears the store with the rest of the
runtime state.
"""

from __future__ import annotations

import sqlite3
from contextlib import closing
from datetime import UTC, datetime
from typing import Any

from google.adk.tools.tool_context import ToolContext

from . import data
from .config import settings
from .models import normalize_incident_id
from .pii import redact_persisted_text

# Identity for programmatic calls outside an ADK session (scripts, tests).
_ANONYMOUS = "direct-call"
_MAX_NOTE_LENGTH = 2000
_RECALL_LIMIT = 20

_SCHEMA = """
CREATE TABLE IF NOT EXISTS incident_notes (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          TEXT NOT NULL,
    user_id     TEXT NOT NULL,
    incident_id TEXT NOT NULL,
    note        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_notes_user_incident ON incident_notes (user_id, incident_id);
"""


def memory_db_path() -> str:
    """Return the long-term memory database path inside the disposable state dir."""
    settings.state_dir.mkdir(parents=True, exist_ok=True)
    return str(settings.state_dir / "memory.db")


def _connect() -> sqlite3.Connection:
    connection = sqlite3.connect(memory_db_path(), timeout=5)
    connection.row_factory = sqlite3.Row
    connection.executescript(_SCHEMA)
    return connection


def _user(tool_context: ToolContext | None) -> str:
    return getattr(tool_context, "user_id", None) or _ANONYMOUS


def save_incident_note(incident_id: str, note: str, tool_context: ToolContext | None = None) -> dict[str, Any]:
    """Save a durable note about an incident for future conversations.

    Use this when you learn something worth remembering across sessions: a fix that
    was attempted, its outcome, or context the next on-call engineer needs.

    Args:
        incident_id: The incident the note belongs to, e.g. ``INC-002``.
        note: One short, factual sentence (attempted action, outcome, decision).

    Returns:
        The saved note (with PII redacted), or an ``error`` for invalid input.
    """
    normalized = normalize_incident_id(incident_id)
    if normalized is None:
        return {"error": f"Invalid incident id {incident_id!r}; expected an id like INC-002."}
    if data.get_incident(normalized) is None:
        return {"error": f"No incident found with id {normalized!r}; refusing to create orphaned memory."}
    cleaned = note.strip()
    if not cleaned:
        return {"error": "Refusing to save an empty note."}
    if len(cleaned) > _MAX_NOTE_LENGTH:
        return {"error": f"Note is too long ({len(cleaned)} chars); keep it under {_MAX_NOTE_LENGTH}."}
    # Memory is a persistence boundary: redact before the write, not after the read.
    redacted = redact_persisted_text(cleaned)
    timestamp = datetime.now(UTC).isoformat(timespec="seconds").replace("+00:00", "Z")
    with closing(_connect()) as connection:
        connection.execute(
            "INSERT INTO incident_notes (ts, user_id, incident_id, note) VALUES (?, ?, ?, ?)",
            (timestamp, _user(tool_context), normalized, redacted),
        )
        connection.commit()
    return {"saved": {"incident_id": normalized, "ts": timestamp, "note": redacted}}


def recall_incident_context(incident_id: str = "", tool_context: ToolContext | None = None) -> dict[str, Any]:
    """Recall your saved notes from previous conversations, newest first.

    Call this at the start of an investigation to pick up where the last
    conversation left off.

    Args:
        incident_id: Optional incident to filter by, e.g. ``INC-002``. Leave empty
            to recall recent notes across all incidents.

    Returns:
        A dict with ``count`` and ``notes`` (ts, incident_id, note) for the current user.
    """
    normalized: str | None = None
    if incident_id:
        normalized = normalize_incident_id(incident_id)
        if normalized is None:
            return {"error": f"Invalid incident id {incident_id!r}; expected an id like INC-002."}
    query = "SELECT ts, incident_id, note FROM incident_notes WHERE user_id = ?"
    params: list[str] = [_user(tool_context)]
    if normalized:
        query += " AND incident_id = ?"
        params.append(normalized)
    query += " ORDER BY id DESC LIMIT ?"
    with closing(_connect()) as connection:
        rows = connection.execute(query, (*params, _RECALL_LIMIT)).fetchall()
    notes = [{"ts": row["ts"], "incident_id": row["incident_id"], "note": row["note"]} for row in rows]
    return {"count": len(notes), "notes": notes}


# Long-term memory stays local to the agent (per-user store) even when the read
# tools are served through the governed MCP route.
MEMORY_TOOLS = [recall_incident_context, save_incident_note]
