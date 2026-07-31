"""Data-access layer over the bundled AgentOps Agent dataset (SQLite + runbooks).

Pure, side-effect-light helpers the tools (Ch. 3.1) build on. Kept separate from the
tool functions so it can be unit-tested directly against ``agents/data/incidents.db``.
"""

from __future__ import annotations

import os
import shutil
import sqlite3
import tempfile
from collections.abc import Iterator
from contextlib import contextmanager
from datetime import UTC, datetime
from pathlib import Path

from .config import settings
from .models import CURRENT_AUDIT_SCHEMA_VERSION, AuditEntry, Incident, IncidentStatus, Service, normalize_slug


class DataAccessError(RuntimeError):
    """Dataset access failed after crossing the trusted-data boundary."""


_RUNTIME_TABLES = frozenset({"audit_log", "incidents", "services"})
_AUDIT_IDEMPOTENCY_INDEX = "uq_audit_log_idempotency"
_AUDIT_IDEMPOTENCY_KEY_ROWS = (
    (0, "invocation_id", 0, "BINARY"),
    (1, "action", 0, "BINARY"),
    (2, "target", 0, "BINARY"),
)
_FUTURE_AUDIT_SCHEMA_GUIDANCE = (
    "The runtime database contains audit rows with an unsupported application schema. "
    "Upgrade the application or select a compatible snapshot before reading or changing this state"
)


def _reject_future_audit_schema(connection: sqlite3.Connection) -> None:
    """Reject audit rows this application cannot parse without changing state."""
    version_column_exists = (
        connection.execute(
            """
            SELECT 1
            FROM pragma_table_info('audit_log')
            WHERE name = 'schema_version'
            """
        ).fetchone()
        is not None
    )
    if not version_column_exists:
        return
    unsupported = connection.execute(
        """
        SELECT schema_version, typeof(schema_version)
        FROM audit_log
        WHERE typeof(schema_version) != 'integer'
           OR schema_version < 1
           OR schema_version > ?
        ORDER BY id
        LIMIT 1
        """,
        (CURRENT_AUDIT_SCHEMA_VERSION,),
    ).fetchone()
    if unsupported is not None:
        raise DataAccessError(
            f"{_FUTURE_AUDIT_SCHEMA_GUIDANCE}: found {unsupported[0]!r} "
            f"with SQLite type {unsupported[1]!r}; supported versions are integers from "
            f"1 through {CURRENT_AUDIT_SCHEMA_VERSION}."
        )


def _audit_idempotency_index_is_current(connection: sqlite3.Connection) -> bool:
    """Return whether the named index enforces the exact bytewise key contract."""
    index = connection.execute(
        """
        SELECT "unique", partial
        FROM pragma_index_list('audit_log')
        WHERE name = 'uq_audit_log_idempotency'
        """
    ).fetchone()
    if index is None or index[0] != 1 or index[1] != 0:
        return False
    key_rows = tuple(
        (row[0], row[1], row[2], row[3])
        for row in connection.execute(
            """
            SELECT seqno, name, "desc", coll
            FROM pragma_index_xinfo('uq_audit_log_idempotency')
            WHERE key = 1
            ORDER BY seqno
            """
        )
    )
    return key_rows == _AUDIT_IDEMPOTENCY_KEY_ROWS


def db_path() -> Path:
    """Return a writable runtime copy of the committed SQLite seed."""
    destination = settings.state_dir / "incidents.db"
    if destination.exists():
        return destination
    source = settings.data_dir / "incidents.db"
    if not source.is_file():
        raise DataAccessError(f"Seed database is missing: {source}")
    settings.state_dir.mkdir(parents=True, exist_ok=True)
    temporary: Path | None = None
    try:
        # Publish a complete copy atomically. Two workers can race safely: the
        # hard link is an exclusive create, so neither can overwrite live state.
        with (
            source.open("rb") as seed,
            tempfile.NamedTemporaryFile(
                dir=settings.state_dir,
                prefix=".incidents-",
                suffix=".tmp",
                delete=False,
            ) as target,
        ):
            temporary = Path(target.name)
            shutil.copyfileobj(seed, target)
            target.flush()
            os.fsync(target.fileno())
        os.link(temporary, destination)
    except FileExistsError:
        # Another local worker initialized the same state directory first.
        pass
    except OSError as error:
        raise DataAccessError(f"Could not initialize runtime database: {destination}") from error
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)
    return destination


def probe_runtime_database() -> Path:
    """Verify the initialized runtime SQLite database read-only."""
    path = settings.state_dir / "incidents.db"
    if not path.is_file():
        raise DataAccessError(f"Runtime database is not initialized: {path}")
    try:
        connection = sqlite3.connect(f"{path.resolve().as_uri()}?mode=ro", uri=True, timeout=5)
        try:
            connection.execute("PRAGMA query_only = ON")
            integrity = connection.execute("PRAGMA quick_check(1)").fetchone()
            if integrity is None or integrity[0] != "ok":
                status = "no result" if integrity is None else str(integrity[0])
                raise DataAccessError(f"Runtime database integrity check failed for {path.name}: {status}")
            tables = {
                row[0]
                for row in connection.execute(
                    """
                    SELECT name
                    FROM sqlite_schema
                    WHERE type = 'table' AND name IN ('audit_log', 'incidents', 'services')
                    """
                )
            }
            version_column = connection.execute(
                """
                SELECT type, "notnull", dflt_value
                FROM pragma_table_info('audit_log')
                WHERE name = 'schema_version'
                """
            ).fetchone()
            idempotency_index_is_current = _audit_idempotency_index_is_current(connection)
            _reject_future_audit_schema(connection)
        finally:
            connection.close()
    except DataAccessError:
        raise
    except (OSError, sqlite3.Error) as error:
        raise DataAccessError(f"Runtime database read-only probe failed for {path.name}") from error
    missing = _RUNTIME_TABLES - tables
    if missing:
        raise DataAccessError(f"Runtime database is missing required tables: {', '.join(sorted(missing))}")
    if (
        version_column is None
        or version_column[0].upper() != "INTEGER"
        or version_column[1] != 1
        or version_column[2] != str(CURRENT_AUDIT_SCHEMA_VERSION)
        or not idempotency_index_is_current
    ):
        raise DataAccessError(
            "Runtime database audit schema is not prepared: expected the current schema_version "
            f"and exact ascending BINARY unique index {_AUDIT_IDEMPOTENCY_INDEX!r}"
        )
    return path


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
    """Return the full markdown of a runbook by slug, or ``None`` if it does not exist.

    The slug is model-controlled, so it is validated against ``_SLUG`` before touching the
    filesystem: a malformed or path-traversal slug is treated as "not found" rather than read.
    """
    normalized = normalize_slug(slug)
    if normalized is None:
        return None
    path = runbook_path(normalized)
    return path.read_text(encoding="utf-8") if path.is_file() else None


def logs_dir() -> Path:
    """Return the directory holding deterministic sample service logs."""
    return settings.data_dir / "logs"


def read_service_logs(service: str) -> list[str] | None:
    """Return a service's log lines, or ``None`` for an invalid/unknown service."""
    normalized = normalize_slug(service)
    if normalized is None:
        return None
    path = logs_dir() / f"{normalized}.log"
    return path.read_text(encoding="utf-8").splitlines() if path.is_file() else None


@contextmanager
def _connect() -> Iterator[sqlite3.Connection]:
    """Open a constrained SQLite connection and wrap database errors with context."""
    path = db_path()
    connection = sqlite3.connect(path, timeout=5)
    try:
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA foreign_keys = ON")
        connection.execute("PRAGMA busy_timeout = 5000")
        yield connection
    except sqlite3.Error as error:
        connection.rollback()
        raise DataAccessError(f"SQLite operation failed for {path.name}") from error
    finally:
        connection.close()


@contextmanager
def _connect_for_read() -> Iterator[sqlite3.Connection]:
    """Open existing runtime state or the seed read-only without publishing state."""
    runtime = settings.state_dir / "incidents.db"
    path = runtime if runtime.is_file() else settings.data_dir / "incidents.db"
    if not path.is_file():
        raise DataAccessError(f"Database is missing: {path.name}")
    try:
        connection = sqlite3.connect(f"{path.resolve().as_uri()}?mode=ro", uri=True, timeout=5)
    except (OSError, sqlite3.Error) as error:
        raise DataAccessError(f"Could not open database read-only: {path.name}") from error
    try:
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA query_only = ON")
        connection.execute("PRAGMA busy_timeout = 5000")
        _reject_future_audit_schema(connection)
        yield connection
    except sqlite3.Error as error:
        raise DataAccessError(f"SQLite read failed for {path.name}") from error
    finally:
        connection.close()


def prepare_runtime_database() -> Path:
    """Initialize and add safe audit-schema migrations to writable runtime state."""
    path = db_path()
    with _connect() as connection:
        connection.execute("BEGIN IMMEDIATE")
        _reject_future_audit_schema(connection)
        version_column = connection.execute(
            """
            SELECT type, "notnull", dflt_value
            FROM pragma_table_info('audit_log')
            WHERE name = 'schema_version'
            """
        ).fetchone()
        if version_column is None:
            connection.execute(
                """
                ALTER TABLE audit_log
                ADD COLUMN schema_version INTEGER NOT NULL DEFAULT 1 CHECK (schema_version >= 1)
                """
            )
            version_column = connection.execute(
                """
                SELECT type, "notnull", dflt_value
                FROM pragma_table_info('audit_log')
                WHERE name = 'schema_version'
                """
            ).fetchone()
        if (
            version_column is None
            or version_column["type"].upper() != "INTEGER"
            or version_column["notnull"] != 1
            or version_column["dflt_value"] != str(CURRENT_AUDIT_SCHEMA_VERSION)
        ):
            connection.rollback()
            raise DataAccessError(
                "Runtime audit schema_version has an unexpected definition; "
                "inspect the runtime database before retrying the migration"
            )

        index_exists = (
            connection.execute(
                """
                SELECT 1
                FROM pragma_index_list('audit_log')
                WHERE name = 'uq_audit_log_idempotency'
                """
            ).fetchone()
            is not None
        )
        if index_exists:
            if not _audit_idempotency_index_is_current(connection):
                connection.rollback()
                raise DataAccessError(
                    f"Runtime index {_AUDIT_IDEMPOTENCY_INDEX!r} has an unexpected definition; "
                    "inspect the runtime database before retrying the migration"
                )
            connection.commit()
            return path

        duplicate = connection.execute(
            """
            SELECT invocation_id, action, target, COUNT(*) AS row_count
            FROM audit_log
            GROUP BY invocation_id, action, target
            HAVING COUNT(*) > 1
            ORDER BY row_count DESC, invocation_id, action, target
            LIMIT 1
            """
        ).fetchone()
        if duplicate is not None:
            connection.rollback()
            raise DataAccessError(
                "Runtime database contains a duplicate audit idempotency key: "
                f"invocation_id={duplicate['invocation_id']!r}, action={duplicate['action']!r}, "
                f"target={duplicate['target']!r} has {duplicate['row_count']} rows. "
                "Export and reconcile duplicate audit rows before retrying the migration"
            )
        connection.execute(
            """
            CREATE UNIQUE INDEX uq_audit_log_idempotency
            ON audit_log (invocation_id, action, target)
            """
        )
        connection.commit()
    return path


@contextmanager
def _connect_for_write() -> Iterator[sqlite3.Connection]:
    """Prepare writable state, then open the caller's mutation connection."""
    prepare_runtime_database()
    with _connect() as connection:
        yield connection


def list_incidents(status: IncidentStatus | None = None, service: str | None = None) -> list[Incident]:
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
    with _connect_for_read() as connection:
        return [Incident.model_validate(dict(row)) for row in connection.execute(query, params)]


def get_incident(incident_id: str) -> Incident | None:
    """Return a single incident by id (e.g. ``INC-001``), or ``None`` if unknown."""
    with _connect_for_read() as connection:
        row = connection.execute("SELECT * FROM incidents WHERE id = ?", (incident_id,)).fetchone()
    return Incident.model_validate(dict(row)) if row is not None else None


def list_services() -> list[Service]:
    """Return every watched service with its current status and owning team."""
    with _connect_for_read() as connection:
        return [Service.model_validate(dict(row)) for row in connection.execute("SELECT * FROM services ORDER BY name")]


def get_service(name: str) -> Service | None:
    """Return a single service by name, or ``None`` if unknown."""
    with _connect_for_read() as connection:
        row = connection.execute("SELECT * FROM services WHERE name = ?", (name,)).fetchone()
    return Service.model_validate(dict(row)) if row is not None else None


def _utcnow() -> str:
    """Return the current time as an ISO-8601 UTC string (e.g. 2026-07-05T09:15:00Z)."""
    return datetime.now(UTC).isoformat(timespec="seconds").replace("+00:00", "Z")


def _audit_for_idempotency_key(
    connection: sqlite3.Connection,
    *,
    invocation_id: str,
    action: str,
    target: str,
) -> AuditEntry | None:
    """Return the first audit row for one logical write, if it already ran."""
    row = connection.execute(
        """
        SELECT *
        FROM audit_log
        WHERE invocation_id = ? AND action = ? AND target = ?
        ORDER BY id
        LIMIT 1
        """,
        (invocation_id, action, target),
    ).fetchone()
    return AuditEntry.model_validate(dict(row)) if row is not None else None


def _append_audit(
    connection: sqlite3.Connection,
    *,
    actor: str,
    approved_by: str,
    rationale: str,
    context_summary: str,
    session_id: str,
    invocation_id: str,
    action: str,
    target: str,
    detail: str,
) -> AuditEntry:
    """Append an audit row using the caller's transaction."""
    timestamp = _utcnow()
    cursor = connection.execute(
        """
        INSERT INTO audit_log
            (schema_version, ts, actor, approved_by, rationale, context_summary,
             session_id, invocation_id, action, target, detail)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        """,
        (
            CURRENT_AUDIT_SCHEMA_VERSION,
            timestamp,
            actor,
            approved_by,
            rationale,
            context_summary,
            session_id,
            invocation_id,
            action,
            target,
            detail,
        ),
    )
    entry_id = cursor.lastrowid
    if entry_id is None:
        raise DataAccessError("SQLite did not return an audit entry id")
    return AuditEntry(
        id=entry_id,
        schema_version=CURRENT_AUDIT_SCHEMA_VERSION,
        ts=timestamp,
        actor=actor,
        approved_by=approved_by,
        rationale=rationale,
        context_summary=context_summary,
        session_id=session_id,
        invocation_id=invocation_id,
        action=action,
        target=target,
        detail=detail,
    )


def append_audit(
    *,
    actor: str,
    approved_by: str,
    rationale: str,
    context_summary: str,
    session_id: str,
    invocation_id: str,
    action: str,
    target: str,
    detail: str,
) -> AuditEntry:
    """Append one audit entry, or return the row for a replayed logical write."""
    with _connect_for_write() as connection:
        connection.execute("BEGIN IMMEDIATE")
        existing = _audit_for_idempotency_key(
            connection,
            invocation_id=invocation_id,
            action=action,
            target=target,
        )
        if existing is not None:
            return existing
        entry = _append_audit(
            connection,
            actor=actor,
            approved_by=approved_by,
            rationale=rationale,
            context_summary=context_summary,
            session_id=session_id,
            invocation_id=invocation_id,
            action=action,
            target=target,
            detail=detail,
        )
        connection.commit()
    return entry


def _restart_context(connection: sqlite3.Connection, name: str) -> str | None:
    """Read restart decision context while the caller holds the write lock."""
    service = connection.execute("SELECT status FROM services WHERE name = ?", (name,)).fetchone()
    if service is None:
        return None
    incident_ids = [
        row["id"]
        for row in connection.execute(
            """
            SELECT id
            FROM incidents
            WHERE service = ? AND status != 'resolved'
            ORDER BY opened_at DESC
            """,
            (name,),
        )
    ]
    return f"service {name} was {service['status']}; open incidents: {', '.join(incident_ids) or 'none'}"


def _resolution_context(connection: sqlite3.Connection, incident_id: str) -> str | None:
    """Read resolution decision context while the caller holds the write lock."""
    incident = connection.execute(
        """
        SELECT service, title, severity, status, runbook
        FROM incidents
        WHERE id = ?
        """,
        (incident_id,),
    ).fetchone()
    if incident is None or incident["status"] == IncidentStatus.RESOLVED.value:
        return None
    return (
        f"incident {incident_id} ({incident['severity']}, {incident['status']}) on {incident['service']}: "
        f"{incident['title']}; runbook {incident['runbook']}"
    )


def restart_service_with_audit(
    name: str,
    *,
    actor: str,
    approved_by: str,
    rationale: str,
    session_id: str,
    invocation_id: str,
) -> AuditEntry | None:
    """Return a prior result or restart and audit once under the write lock."""
    with _connect_for_write() as connection:
        connection.execute("BEGIN IMMEDIATE")
        existing = _audit_for_idempotency_key(
            connection,
            invocation_id=invocation_id,
            action="restart_service",
            target=name,
        )
        if existing is not None:
            return existing
        context_summary = _restart_context(connection, name)
        if context_summary is None:
            return None
        cursor = connection.execute("UPDATE services SET status = 'operational' WHERE name = ?", (name,))
        if cursor.rowcount == 0:
            return None
        entry = _append_audit(
            connection,
            actor=actor,
            approved_by=approved_by,
            rationale=rationale,
            context_summary=context_summary,
            session_id=session_id,
            invocation_id=invocation_id,
            action="restart_service",
            target=name,
            detail="service restarted and marked operational (mock)",
        )
        connection.commit()
    return entry


def resolve_incident_with_audit(
    incident_id: str,
    *,
    actor: str,
    approved_by: str,
    rationale: str,
    session_id: str,
    invocation_id: str,
) -> AuditEntry | None:
    """Return a prior result or resolve and audit once under the write lock."""
    with _connect_for_write() as connection:
        connection.execute("BEGIN IMMEDIATE")
        existing = _audit_for_idempotency_key(
            connection,
            invocation_id=invocation_id,
            action="resolve_incident",
            target=incident_id,
        )
        if existing is not None:
            return existing
        context_summary = _resolution_context(connection, incident_id)
        if context_summary is None:
            return None
        cursor = connection.execute(
            "UPDATE incidents SET status = 'resolved', resolved_at = ? WHERE id = ? AND status != 'resolved'",
            (_utcnow(), incident_id),
        )
        if cursor.rowcount == 0:
            return None
        entry = _append_audit(
            connection,
            actor=actor,
            approved_by=approved_by,
            rationale=rationale,
            context_summary=context_summary,
            session_id=session_id,
            invocation_id=invocation_id,
            action="resolve_incident",
            target=incident_id,
            detail="incident resolved (mock)",
        )
        connection.commit()
    return entry
