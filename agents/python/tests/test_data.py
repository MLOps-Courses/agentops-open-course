"""Focused tests for runtime-state initialization and atomic data mutations."""

import sqlite3
from contextlib import closing
from types import SimpleNamespace
from typing import cast

import pytest

from agent import data


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
    assert data.resolve_incident_with_audit("INC-003", **identity) is None


def test_atomic_mutations_derive_context_from_locked_current_rows() -> None:
    with closing(sqlite3.connect(data.db_path())) as connection:
        connection.execute("UPDATE services SET status = 'degraded' WHERE name = 'inventory'")
        connection.execute(
            """
            UPDATE incidents
            SET status = 'investigating', title = 'Fresh transaction state'
            WHERE id = 'INC-002'
            """
        )
        connection.commit()
    identity = {
        "actor": "test",
        "approved_by": "engineer",
        "rationale": "test approval",
        "session_id": "session",
        "invocation_id": "invocation",
    }

    restart = data.restart_service_with_audit("inventory", **identity)
    assert restart is not None
    assert "service inventory was degraded" in restart.context_summary
    assert "INC-002" in restart.context_summary

    resolution = data.resolve_incident_with_audit("INC-002", **identity)
    assert resolution is not None
    assert "(SEV1, investigating)" in resolution.context_summary
    assert "Fresh transaction state" in resolution.context_summary


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
                competitor.execute("UPDATE services SET status = 'degraded' WHERE name = 'inventory'")
        return context

    monkeypatch.setattr(data, "_restart_context", try_competing_write)
    entry = data.restart_service_with_audit(
        "inventory",
        actor="test",
        approved_by="engineer",
        rationale="test approval",
        session_id="session",
        invocation_id="invocation",
    )
    assert competing_write_attempted is True
    assert entry is not None
    assert "service inventory was down" in entry.context_summary


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
            target="checkout",
            detail="test",
        )
