"""Focused tests for runtime-state initialization and atomic data mutations."""

import sqlite3
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


def test_invalid_log_slug_is_not_read() -> None:
    assert data.read_service_logs("../checkout") is None


def test_atomic_mutations_return_none_for_unknown_or_resolved_rows() -> None:
    identity = {
        "actor": "test",
        "approved_by": "engineer",
        "rationale": "test approval",
        "context_summary": "test context",
        "session_id": "session",
        "invocation_id": "invocation",
    }
    assert data.restart_service_with_audit("unknown", **identity) is None
    assert data.resolve_incident_with_audit("INC-003", **identity) is None


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
