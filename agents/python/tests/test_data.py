"""Focused tests for runtime-state initialization and atomic data mutations."""

import sqlite3
from contextlib import closing
from pathlib import Path
from types import SimpleNamespace
from typing import cast

import pytest
from pydantic import ValidationError

from agent import data
from agent.models import CURRENT_AUDIT_SCHEMA_VERSION, AuditEntry
from tests.domain import REFERENCE_DOMAIN

_CHECKOUT = REFERENCE_DOMAIN.services.checkout
_INVENTORY = REFERENCE_DOMAIN.services.inventory
_INVENTORY_INCIDENT = REFERENCE_DOMAIN.incidents.inventory_down
_RESOLVED_INCIDENT = REFERENCE_DOMAIN.incidents.payments_errors


def _legacy_runtime_database() -> tuple[sqlite3.Connection, Path]:
    path = data.db_path()
    connection = sqlite3.connect(path)
    connection.execute("DROP INDEX IF EXISTS uq_audit_log_idempotency")
    connection.execute("ALTER TABLE audit_log DROP COLUMN schema_version")
    return connection, path


def _insert_legacy_audit(connection: sqlite3.Connection, *, invocation_id: str, detail: str) -> None:
    connection.execute(
        """
        INSERT INTO audit_log
            (ts, actor, approved_by, rationale, context_summary, session_id, invocation_id, action, target, detail)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        """,
        (
            "2026-07-29T08:00:00Z",
            "test",
            "engineer",
            "legacy approval",
            "legacy context",
            "legacy-session",
            invocation_id,
            "restart_service",
            _INVENTORY,
            detail,
        ),
    )


def _runtime_database_with_nocase_index() -> Path:
    path = data.db_path()
    with closing(sqlite3.connect(path)) as connection:
        connection.execute("DROP INDEX uq_audit_log_idempotency")
        connection.execute(
            """
            CREATE UNIQUE INDEX uq_audit_log_idempotency
            ON audit_log (invocation_id COLLATE NOCASE, action, target)
            """
        )
        connection.commit()
    return path


def test_db_path_fails_when_seed_is_missing(tmp_path, monkeypatch) -> None:
    monkeypatch.setattr(data.settings, "data_dir", tmp_path / "missing")
    monkeypatch.setattr(data.settings, "state_dir", tmp_path / "state")
    with pytest.raises(data.DataAccessError, match="Seed database is missing"):
        data.db_path()


def test_db_path_keeps_the_winner_of_an_initialization_race(monkeypatch) -> None:
    destination = data.settings.state_dir / "incidents.db"

    def competing_link(source, target) -> None:
        del source
        target.write_bytes(b"winner")
        raise FileExistsError

    monkeypatch.setattr(data.os, "link", competing_link)
    assert data.db_path() == destination
    assert destination.read_bytes() == b"winner"
    assert not list(data.settings.state_dir.glob(".incidents-*.tmp"))


def test_db_path_wraps_filesystem_failures(monkeypatch) -> None:
    def reject_link(source, target) -> None:
        del source, target
        raise PermissionError("read-only")

    monkeypatch.setattr(data.os, "link", reject_link)
    with pytest.raises(data.DataAccessError, match="Could not initialize runtime database"):
        data.db_path()
    assert not list(data.settings.state_dir.glob(".incidents-*.tmp"))


def test_runtime_probe_initializes_only_for_the_state_owner() -> None:
    destination = data.settings.state_dir / "incidents.db"
    with pytest.raises(data.DataAccessError, match="not initialized"):
        data.probe_runtime_database()
    assert not destination.exists()

    assert data.db_path() == destination
    assert destination.is_file()
    before = destination.read_bytes()
    before_mtime = destination.stat().st_mtime_ns
    assert data.probe_runtime_database() == destination
    assert destination.read_bytes() == before
    assert destination.stat().st_mtime_ns == before_mtime


def test_runtime_probe_rejects_corrupt_state() -> None:
    data.settings.state_dir.mkdir(parents=True)
    destination = data.settings.state_dir / "incidents.db"
    destination.write_text("not a SQLite database", encoding="utf-8")
    before = destination.read_bytes()
    with pytest.raises(data.DataAccessError, match="read-only probe failed"):
        data.probe_runtime_database()
    assert destination.read_bytes() == before


def test_runtime_probe_wraps_unreadable_state(monkeypatch) -> None:
    data.db_path()

    def reject_connection(*_args, **_kwargs):
        raise PermissionError("read-only")

    monkeypatch.setattr(data.sqlite3, "connect", reject_connection)
    with pytest.raises(data.DataAccessError, match="read-only probe failed"):
        data.probe_runtime_database()


def test_runtime_probe_rejects_an_incomplete_schema() -> None:
    data.settings.state_dir.mkdir(parents=True)
    destination = data.settings.state_dir / "incidents.db"
    with closing(sqlite3.connect(destination)) as connection:
        connection.execute("CREATE TABLE services (name TEXT PRIMARY KEY)")
        connection.commit()

    with pytest.raises(data.DataAccessError, match="missing required tables") as excinfo:
        data.probe_runtime_database()
    assert "audit_log" in str(excinfo.value)
    assert "incidents" in str(excinfo.value)


def test_runtime_probe_rejects_legacy_audit_schema_without_migrating_it() -> None:
    connection, path = _legacy_runtime_database()
    with closing(connection):
        connection.commit()
    before = path.read_bytes()
    before_mtime = path.stat().st_mtime_ns

    with pytest.raises(data.DataAccessError, match="audit schema is not prepared"):
        data.probe_runtime_database()
    assert path.read_bytes() == before
    assert path.stat().st_mtime_ns == before_mtime


def test_runtime_probe_rejects_nocase_idempotency_index_without_writing() -> None:
    path = _runtime_database_with_nocase_index()
    before = path.read_bytes()
    before_mtime = path.stat().st_mtime_ns

    with pytest.raises(data.DataAccessError, match="ascending BINARY unique index"):
        data.probe_runtime_database()
    assert path.read_bytes() == before
    assert path.stat().st_mtime_ns == before_mtime


def test_future_audit_schema_is_rejected_before_probe_read_or_migration() -> None:
    path = data.db_path()
    with closing(sqlite3.connect(path)) as connection:
        connection.execute(
            """
            INSERT INTO audit_log
                (schema_version, ts, actor, approved_by, rationale, context_summary,
                 session_id, invocation_id, action, target, detail)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                CURRENT_AUDIT_SCHEMA_VERSION + 1,
                "2026-07-31T08:00:00Z",
                "future-agent",
                "engineer",
                "future approval",
                "future context",
                "future-session",
                "future-invocation",
                "restart_service",
                _INVENTORY,
                "future detail",
            ),
        )
        connection.commit()
    before = path.read_bytes()
    guidance = "Upgrade the application or select a compatible snapshot"

    with pytest.raises(data.DataAccessError, match=guidance):
        data.probe_runtime_database()
    assert path.read_bytes() == before

    with pytest.raises(data.DataAccessError, match=guidance):
        data.list_incidents()
    assert path.read_bytes() == before

    with pytest.raises(data.DataAccessError, match=guidance):
        data.prepare_runtime_database()
    assert path.read_bytes() == before


def test_invalid_audit_schema_type_is_rejected_before_probe_read_or_migration() -> None:
    path = data.db_path()
    with closing(sqlite3.connect(path)) as connection:
        connection.execute("PRAGMA ignore_check_constraints = ON")
        connection.execute(
            """
            INSERT INTO audit_log
                (schema_version, ts, actor, approved_by, rationale, context_summary,
                 session_id, invocation_id, action, target, detail)
            VALUES ('future', '2026-07-31T08:00:00Z', 'future-agent', 'engineer',
                    'invalid schema fixture', 'future context', 'invalid-session',
                    'invalid-invocation', 'restart_service', ?, 'future detail')
            """,
            (_INVENTORY,),
        )
        connection.commit()
    before = path.read_bytes()
    guidance = "Upgrade the application or select a compatible snapshot"

    with pytest.raises(data.DataAccessError, match=guidance):
        data.probe_runtime_database()
    assert path.read_bytes() == before

    with pytest.raises(data.DataAccessError, match=guidance):
        data.list_incidents()
    assert path.read_bytes() == before

    with pytest.raises(data.DataAccessError, match=guidance):
        data.prepare_runtime_database()
    assert path.read_bytes() == before


def test_prepare_runtime_database_migrates_legacy_state_without_touching_seed() -> None:
    seed = data.settings.data_dir / "incidents.db"
    seed_before = seed.read_bytes()
    seed_mtime = seed.stat().st_mtime_ns
    connection, path = _legacy_runtime_database()
    with closing(connection):
        _insert_legacy_audit(connection, invocation_id="legacy-invocation", detail="first delivery")
        connection.commit()

    assert data.prepare_runtime_database() == path
    with closing(sqlite3.connect(path)) as migrated:
        versions = migrated.execute("SELECT schema_version FROM audit_log").fetchall()
        version_default = migrated.execute(
            "SELECT dflt_value FROM pragma_table_info('audit_log') WHERE name = 'schema_version'"
        ).fetchone()
        unique_index = migrated.execute(
            """
            SELECT "unique"
            FROM pragma_index_list('audit_log')
            WHERE name = 'uq_audit_log_idempotency'
            """
        ).fetchone()
        index_key_rows = tuple(
            (row[0], row[1], row[2], row[3])
            for row in migrated.execute(
                """
                SELECT seqno, name, "desc", coll
                FROM pragma_index_xinfo('uq_audit_log_idempotency')
                WHERE key = 1
                ORDER BY seqno
                """
            )
        )

    assert versions == [(CURRENT_AUDIT_SCHEMA_VERSION,)]
    assert version_default == (str(CURRENT_AUDIT_SCHEMA_VERSION),)
    assert unique_index == (1,)
    assert index_key_rows == (
        (0, "invocation_id", 0, "BINARY"),
        (1, "action", 0, "BINARY"),
        (2, "target", 0, "BINARY"),
    )
    assert seed.read_bytes() == seed_before
    assert seed.stat().st_mtime_ns == seed_mtime


def test_prepare_runtime_database_rejects_duplicate_legacy_keys_atomically() -> None:
    connection, path = _legacy_runtime_database()
    with closing(connection):
        _insert_legacy_audit(connection, invocation_id="duplicate-invocation", detail="first delivery")
        _insert_legacy_audit(connection, invocation_id="duplicate-invocation", detail="second delivery")
        connection.commit()

    with pytest.raises(data.DataAccessError, match="duplicate audit idempotency key") as excinfo:
        data.prepare_runtime_database()
    assert "duplicate-invocation" in str(excinfo.value)
    assert "2 rows" in str(excinfo.value)
    assert "reconcile" in str(excinfo.value)

    with closing(sqlite3.connect(path)) as unchanged:
        columns = {row[1] for row in unchanged.execute("PRAGMA table_info(audit_log)")}
        named_index = unchanged.execute(
            """
            SELECT name
            FROM pragma_index_list('audit_log')
            WHERE name = 'uq_audit_log_idempotency'
            """
        ).fetchone()
    assert "schema_version" not in columns
    assert named_index is None


def test_prepare_runtime_database_is_idempotent_on_current_schema() -> None:
    path = data.prepare_runtime_database()
    assert data.prepare_runtime_database() == path
    with closing(sqlite3.connect(path)) as connection:
        version_columns = connection.execute(
            "SELECT COUNT(*) FROM pragma_table_info('audit_log') WHERE name = 'schema_version'"
        ).fetchone()
        named_indexes = connection.execute(
            """
            SELECT COUNT(*)
            FROM pragma_index_list('audit_log')
            WHERE name = 'uq_audit_log_idempotency' AND "unique" = 1
            """
        ).fetchone()
    assert version_columns == (1,)
    assert named_indexes == (1,)


def test_prepare_runtime_database_rejects_a_partial_named_index() -> None:
    path = data.db_path()
    with closing(sqlite3.connect(path)) as connection:
        connection.execute("DROP INDEX uq_audit_log_idempotency")
        connection.execute(
            """
            CREATE UNIQUE INDEX uq_audit_log_idempotency
            ON audit_log (invocation_id, action, target)
            WHERE action = 'restart_service'
            """
        )
        connection.commit()

    with pytest.raises(data.DataAccessError, match="unexpected definition") as excinfo:
        data.prepare_runtime_database()
    assert "uq_audit_log_idempotency" in str(excinfo.value)
    with closing(sqlite3.connect(path)) as connection:
        partial = connection.execute(
            """
            SELECT partial
            FROM pragma_index_list('audit_log')
            WHERE name = 'uq_audit_log_idempotency'
            """
        ).fetchone()
    assert partial == (1,)


def test_prepare_runtime_database_rejects_a_nocase_named_index() -> None:
    path = _runtime_database_with_nocase_index()

    with pytest.raises(data.DataAccessError, match="unexpected definition") as excinfo:
        data.prepare_runtime_database()
    assert "uq_audit_log_idempotency" in str(excinfo.value)
    with closing(sqlite3.connect(path)) as connection:
        collations = tuple(
            row[0]
            for row in connection.execute(
                """
                SELECT coll
                FROM pragma_index_xinfo('uq_audit_log_idempotency')
                WHERE key = 1
                ORDER BY seqno
                """
            )
        )
    assert collations == ("NOCASE", "BINARY", "BINARY")


def test_direct_writers_prepare_runtime_database(monkeypatch) -> None:
    original = data.prepare_runtime_database
    calls = 0

    def tracked_prepare() -> Path:
        nonlocal calls
        calls += 1
        return original()

    monkeypatch.setattr(data, "prepare_runtime_database", tracked_prepare)
    data.append_audit(
        actor="test",
        approved_by="engineer",
        rationale="test approval",
        context_summary="test context",
        session_id="session",
        invocation_id="append-invocation",
        action="noop",
        target=_CHECKOUT,
        detail="test",
    )
    identity = {
        "actor": "test",
        "approved_by": "engineer",
        "rationale": "test approval",
        "session_id": "session",
    }
    assert data.restart_service_with_audit(_INVENTORY, invocation_id="restart-invocation", **identity) is not None
    assert (
        data.resolve_incident_with_audit(_INVENTORY_INCIDENT, invocation_id="resolve-invocation", **identity)
        is not None
    )
    assert calls == 3


def test_invalid_log_slug_is_not_read() -> None:
    assert data.read_service_logs("../checkout") is None


def test_atomic_mutations_return_none_for_unknown_or_resolved_rows() -> None:
    identity = {
        "actor": "test",
        "approved_by": "engineer",
        "rationale": "test approval",
        "session_id": "session",
        "invocation_id": "invocation",
    }
    assert data.restart_service_with_audit("unknown", **identity) is None
    assert data.resolve_incident_with_audit(_RESOLVED_INCIDENT, **identity) is None


def test_atomic_mutations_derive_context_from_locked_current_rows() -> None:
    with closing(sqlite3.connect(data.db_path())) as connection:
        connection.execute("UPDATE services SET status = 'degraded' WHERE name = ?", (_INVENTORY,))
        connection.execute(
            """
            UPDATE incidents
            SET status = 'investigating', title = 'Fresh transaction state'
            WHERE id = ?
            """,
            (_INVENTORY_INCIDENT,),
        )
        connection.commit()
    identity = {
        "actor": "test",
        "approved_by": "engineer",
        "rationale": "test approval",
        "session_id": "session",
        "invocation_id": "invocation",
    }

    restart = data.restart_service_with_audit(_INVENTORY, **identity)
    assert restart is not None
    assert f"service {_INVENTORY} was degraded" in restart.context_summary
    assert _INVENTORY_INCIDENT in restart.context_summary

    resolution = data.resolve_incident_with_audit(_INVENTORY_INCIDENT, **identity)
    assert resolution is not None
    assert "(SEV1, investigating)" in resolution.context_summary
    assert "Fresh transaction state" in resolution.context_summary


def test_restart_replay_returns_original_audit_without_reapplying_mutation() -> None:
    identity = {
        "actor": "test",
        "approved_by": "engineer",
        "rationale": "test approval",
        "session_id": "session",
        "invocation_id": "invocation",
    }
    first = data.restart_service_with_audit(_INVENTORY, **identity)
    assert first is not None

    # A later event changes the service again. Re-delivering the old invocation
    # must return its evidence without overwriting this newer state.
    with closing(sqlite3.connect(data.db_path())) as connection:
        connection.execute("UPDATE services SET status = 'degraded' WHERE name = ?", (_INVENTORY,))
        connection.commit()

    replay = data.restart_service_with_audit(_INVENTORY, **identity)
    assert replay == first
    service = data.get_service(_INVENTORY)
    assert service is not None
    assert service.status == "degraded"
    with closing(sqlite3.connect(data.db_path())) as connection:
        count = connection.execute(
            """
            SELECT COUNT(*)
            FROM audit_log
            WHERE invocation_id = ? AND action = 'restart_service' AND target = ?
            """,
            (identity["invocation_id"], _INVENTORY),
        ).fetchone()
    assert count == (1,)


def test_resolve_replay_returns_original_audit_after_incident_is_resolved() -> None:
    identity = {
        "actor": "test",
        "approved_by": "engineer",
        "rationale": "test approval",
        "session_id": "session",
        "invocation_id": "invocation",
    }
    first = data.resolve_incident_with_audit(_INVENTORY_INCIDENT, **identity)
    assert first is not None
    assert data.resolve_incident_with_audit(_INVENTORY_INCIDENT, **identity) == first

    later_invocation = {**identity, "invocation_id": "later-invocation"}
    assert data.resolve_incident_with_audit(_INVENTORY_INCIDENT, **later_invocation) is None
    with closing(sqlite3.connect(data.db_path())) as connection:
        count = connection.execute(
            "SELECT COUNT(*) FROM audit_log WHERE action = 'resolve_incident' AND target = ?",
            (_INVENTORY_INCIDENT,),
        ).fetchone()
    assert count == (1,)


def test_audit_schema_rejects_duplicate_key_and_append_returns_original() -> None:
    entry = data.append_audit(
        actor="test",
        approved_by="engineer",
        rationale="test approval",
        context_summary="test context",
        session_id="session",
        invocation_id="invocation",
        action="noop",
        target=_CHECKOUT,
        detail="test",
    )
    assert entry.schema_version == CURRENT_AUDIT_SCHEMA_VERSION
    assert entry.model_dump(mode="json")["schema_version"] == CURRENT_AUDIT_SCHEMA_VERSION
    with closing(sqlite3.connect(data.db_path())) as connection:
        stored_version = connection.execute(
            "SELECT schema_version FROM audit_log WHERE id = ?",
            (entry.id,),
        ).fetchone()
        version_default = connection.execute(
            "SELECT dflt_value FROM pragma_table_info('audit_log') WHERE name = 'schema_version'"
        ).fetchone()
    assert stored_version == (CURRENT_AUDIT_SCHEMA_VERSION,)
    assert version_default == (str(CURRENT_AUDIT_SCHEMA_VERSION),)

    legacy_payload = entry.model_dump(mode="json")
    legacy_payload.pop("schema_version")
    with pytest.raises(ValidationError, match="schema_version"):
        AuditEntry.model_validate(legacy_payload)
    future_payload = entry.model_dump(mode="json")
    future_payload["schema_version"] = CURRENT_AUDIT_SCHEMA_VERSION + 1
    with pytest.raises(ValidationError, match="schema_version"):
        AuditEntry.model_validate(future_payload)

    replay = data.append_audit(
        actor=entry.actor,
        approved_by=entry.approved_by,
        rationale="a changed replay payload must not replace the first row",
        context_summary=entry.context_summary,
        session_id=entry.session_id,
        invocation_id=entry.invocation_id,
        action=entry.action,
        target=entry.target,
        detail=entry.detail,
    )
    assert replay == entry

    with (
        closing(sqlite3.connect(data.db_path())) as connection,
        pytest.raises(sqlite3.IntegrityError, match="UNIQUE constraint failed"),
    ):
        connection.execute(
            """
            INSERT INTO audit_log
                (ts, actor, approved_by, rationale, context_summary, session_id, invocation_id, action, target, detail)
            SELECT ts, actor, approved_by, rationale, context_summary, session_id, invocation_id, action, target, detail
            FROM audit_log
            WHERE id = ?
            """,
            (entry.id,),
        )


def test_restart_context_is_read_after_acquiring_the_write_lock(monkeypatch) -> None:
    original = data._restart_context  # noqa: SLF001 - transaction-order regression seam
    competing_write_attempted = False

    def try_competing_write(connection, name):
        nonlocal competing_write_attempted
        context = original(connection, name)
        competing_write_attempted = True
        with closing(sqlite3.connect(data.db_path(), timeout=0)) as competitor:
            competitor.execute("PRAGMA busy_timeout = 0")
            with pytest.raises(sqlite3.OperationalError, match="locked"):
                competitor.execute("UPDATE services SET status = 'degraded' WHERE name = ?", (_INVENTORY,))
        return context

    monkeypatch.setattr(data, "_restart_context", try_competing_write)
    entry = data.restart_service_with_audit(
        _INVENTORY,
        actor="test",
        approved_by="engineer",
        rationale="test approval",
        session_id="session",
        invocation_id="invocation",
    )
    assert competing_write_attempted is True
    assert entry is not None
    assert f"service {_INVENTORY} was down" in entry.context_summary


def test_append_audit_rejects_a_missing_row_id() -> None:
    connection = cast("sqlite3.Connection", SimpleNamespace(execute=lambda *_args: SimpleNamespace(lastrowid=None)))
    with pytest.raises(data.DataAccessError, match="audit entry id"):
        data._append_audit(  # noqa: SLF001 - exercises a defensive database boundary
            connection,
            actor="test",
            approved_by="engineer",
            rationale="test approval",
            context_summary="test context",
            session_id="session",
            invocation_id="invocation",
            action="noop",
            target=_CHECKOUT,
            detail="test",
        )
