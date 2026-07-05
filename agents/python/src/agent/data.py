"""Data-access layer over the bundled Ops Copilot dataset (SQLite + runbooks).

Pure, side-effect-light helpers the tools (Ch. 3.1) build on. Kept separate from the
tool functions so it can be unit-tested directly against ``agents/data/incidents.db``.
"""

from __future__ import annotations

import sqlite3
from collections.abc import Iterator
from contextlib import contextmanager
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from .config import settings


def db_path() -> Path:
    """Return the path to the SQLite database inside the configured data directory."""
    return settings.data_dir / "incidents.db"


def runbooks_dir() -> Path:
    """Return the directory holding the runbook knowledge base."""
    return settings.data_dir / "runbooks"


def runbook_path(slug: str) -> Path:
    """Return the path to a runbook markdown file by slug."""
    return runbooks_dir() / f"{slug}.md"


def list_runbook_slugs() -> list[str]:
    """Return the slugs of every runbook in the knowledge base, sorted."""
    return sorted(path.stem for path in runbooks_dir().glob("*.md"))


def read_runbook(slug: str) -> str | None:
    """Return the full markdown of a runbook by slug, or ``None`` if it does not exist."""
    path = runbook_path(slug)
    return path.read_text(encoding="utf-8") if path.is_file() else None


@contextmanager
def _connect() -> Iterator[sqlite3.Connection]:
    """Open a read/write connection with row access by column name; always closed."""
    connection = sqlite3.connect(db_path())
    connection.row_factory = sqlite3.Row
    try:
        yield connection
    finally:
        connection.close()


def list_incidents(status: str | None = None, service: str | None = None) -> list[dict[str, Any]]:
    """Return incidents, newest first, optionally filtered by status and/or service."""
    query = "SELECT * FROM incidents"
    clauses: list[str] = []
    params: list[str] = []
    if status is not None:
        clauses.append("status = ?")
        params.append(status)
    if service is not None:
        clauses.append("service = ?")
        params.append(service)
    if clauses:
        query += " WHERE " + " AND ".join(clauses)
    query += " ORDER BY opened_at DESC"
    with _connect() as connection:
        return [dict(row) for row in connection.execute(query, params)]


def get_incident(incident_id: str) -> dict[str, Any] | None:
    """Return a single incident by id (e.g. ``INC-001``), or ``None`` if unknown."""
    with _connect() as connection:
        row = connection.execute("SELECT * FROM incidents WHERE id = ?", (incident_id,)).fetchone()
    return dict(row) if row is not None else None


def list_services() -> list[dict[str, Any]]:
    """Return every watched service with its current status and owning team."""
    with _connect() as connection:
        return [dict(row) for row in connection.execute("SELECT * FROM services ORDER BY name")]


def get_service(name: str) -> dict[str, Any] | None:
    """Return a single service by name, or ``None`` if unknown."""
    with _connect() as connection:
        row = connection.execute("SELECT * FROM services WHERE name = ?", (name,)).fetchone()
    return dict(row) if row is not None else None


def _utcnow() -> str:
    """Return the current time as an ISO-8601 UTC string (e.g. 2026-07-05T09:15:00Z)."""
    return datetime.now(UTC).isoformat(timespec="seconds").replace("+00:00", "Z")


def set_service_status(name: str, status: str) -> bool:
    """Set a service's status (mock action). Returns True if a service was updated."""
    with _connect() as connection:
        cursor = connection.execute("UPDATE services SET status = ? WHERE name = ?", (status, name))
        connection.commit()
        return cursor.rowcount > 0


def resolve_incident(incident_id: str) -> bool:
    """Mark an open incident resolved with a resolved_at timestamp (mock action).

    Returns True if an unresolved incident was updated, False if it was unknown or already resolved.
    """
    with _connect() as connection:
        cursor = connection.execute(
            "UPDATE incidents SET status = 'resolved', resolved_at = ? WHERE id = ? AND status != 'resolved'",
            (_utcnow(), incident_id),
        )
        connection.commit()
        return cursor.rowcount > 0


def append_audit(actor: str, action: str, target: str, detail: str) -> dict[str, Any]:
    """Append one entry to the audit log and return it (used by mock actions, Ch. 4.5)."""
    timestamp = _utcnow()
    with _connect() as connection:
        cursor = connection.execute(
            "INSERT INTO audit_log (ts, actor, action, target, detail) VALUES (?, ?, ?, ?, ?)",
            (timestamp, actor, action, target, detail),
        )
        connection.commit()
        entry_id = cursor.lastrowid
    return {"id": entry_id, "ts": timestamp, "actor": actor, "action": action, "target": target, "detail": detail}
