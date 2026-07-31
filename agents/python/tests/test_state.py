"""Adversarial tests for versioned state snapshot publication and restore."""

import errno
import json
import os
import shutil
import sqlite3
import subprocess
import sys
import time
from contextlib import closing
from importlib.metadata import PackageNotFoundError
from pathlib import Path

import pytest

from agent import state
from agent.config import settings
from agent.models import CURRENT_AUDIT_SCHEMA_VERSION
from tests.domain import REFERENCE_DOMAIN

_INVENTORY_TERM = REFERENCE_DOMAIN.services.inventory


def _seed_state() -> Path:
    settings.state_dir.mkdir(parents=True)
    shutil.copyfile(settings.data_dir / "incidents.db", settings.state_dir / "incidents.db")
    with closing(sqlite3.connect(settings.state_dir / "runtime.db")) as connection:
        connection.execute("CREATE TABLE sessions (id TEXT PRIMARY KEY)")
        connection.execute("INSERT INTO sessions VALUES ('session-1')")
        connection.commit()
    return settings.state_dir


def _snapshot(tmp_path: Path, monkeypatch) -> Path:
    _seed_state()
    monkeypatch.setenv("AGENT_SOURCE_COMMIT", "a" * 40)
    monkeypatch.setenv("STATE_BACKUP_TIMESTAMP", "20990101T000000Z")
    return state.backup_state(settings.state_dir, tmp_path / "backups")


def _fingerprints(directory: Path) -> dict[str, bytes]:
    return {path.name: path.read_bytes() for path in sorted(directory.glob("*.db"))}


def _generation_fingerprints(directory: Path) -> dict[str, bytes]:
    return {
        path.name: path.read_bytes()
        for path in sorted(directory.iterdir())
        if state._is_state_filename(path.name)  # noqa: SLF001 - restore generation contract
    }


def _child_environment() -> dict[str, str]:
    environment = os.environ.copy()
    environment["PYTHONPATH"] = str(Path(__file__).parents[1] / "src")
    return environment


def _run_restore_child(code: str, snapshot: Path, state_dir: Path) -> subprocess.CompletedProcess[str]:
    return subprocess.run(  # noqa: S603 - fixed interpreter and test-owned arguments
        [sys.executable, "-c", code, str(snapshot), str(state_dir)],
        check=False,
        capture_output=True,
        text=True,
        env=_child_environment(),
    )


def _wait_for_file(path: Path) -> None:
    deadline = time.monotonic() + 5
    while not path.exists():
        if time.monotonic() >= deadline:
            pytest.fail(f"timed out waiting for subprocess marker {path}")
        time.sleep(0.01)


def _read_manifest(snapshot: Path) -> dict:
    return json.loads((snapshot / "manifest.json").read_text(encoding="utf-8"))


def _publish_manifest(snapshot: Path, manifest: dict) -> None:
    manifest_path = snapshot / "manifest.json"
    state._write_json(manifest_path, manifest)  # noqa: SLF001 - signed corruption fixture
    state._write_json(  # noqa: SLF001
        snapshot / ".complete",
        {
            "format_version": state.SNAPSHOT_FORMAT_VERSION,
            "manifest_sha256": state._sha256(manifest_path),  # noqa: SLF001
        },
    )


def _journal_payload(
    *,
    transaction_id: str = "a" * 32,
    phase: str = "prepared",
) -> dict:
    return {
        "format_version": 1,
        "transaction_id": transaction_id,
        "phase": phase,
        "staging_dir": f".restore-staged.{transaction_id}",
        "quarantine_dir": f".restore-quarantine.{transaction_id}",
        "old_inventory": [],
        "new_inventory": [
            {
                "filename": "new.db",
                "sha256": "0" * 64,
                "size_bytes": 0,
            }
        ],
    }


def test_backup_records_versioned_hashed_inventory_and_source(monkeypatch, tmp_path) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    manifest = json.loads((snapshot / "manifest.json").read_text(encoding="utf-8"))
    marker = json.loads((snapshot / ".complete").read_text(encoding="utf-8"))

    assert manifest["format_version"] == state.SNAPSHOT_FORMAT_VERSION
    assert manifest["source"] == {
        "application": "agentops-agent",
        "version": state._application_version(),  # noqa: SLF001 - manifest contract
        "commit": "a" * 40,
    }
    assert [entry["filename"] for entry in manifest["databases"]] == ["incidents.db", "runtime.db"]
    assert all(len(entry["sha256"]) == 64 for entry in manifest["databases"])
    assert all(entry["sqlite"]["schema_sha256"] for entry in manifest["databases"])
    assert marker == {
        "format_version": state.SNAPSHOT_FORMAT_VERSION,
        "manifest_sha256": state._sha256(snapshot / "manifest.json"),  # noqa: SLF001
    }


@pytest.mark.parametrize(
    ("raw_inventory", "role", "message"),
    [
        (None, "old", "invalid old inventory"),
        ([], "new", "invalid new inventory"),
        (["not evidence"], "old", "incomplete old file evidence"),
        ([{"filename": "../old.db", "sha256": "0" * 64, "size_bytes": 1}], "old", "unsafe old filename"),
        (
            [
                {"filename": "old.db", "sha256": "0" * 64, "size_bytes": 1},
                {"filename": "old.db", "sha256": "0" * 64, "size_bytes": 1},
            ],
            "old",
            "repeats old filename",
        ),
        ([{"filename": "old.db", "sha256": "not-a-digest", "size_bytes": 1}], "old", "invalid old SHA-256"),
        ([{"filename": "old.db", "sha256": "0" * 64, "size_bytes": True}], "old", "invalid old size"),
    ],
)
def test_restore_journal_inventory_parser_rejects_incomplete_evidence(raw_inventory, role, message) -> None:
    with pytest.raises(state.StateSnapshotError, match=message):
        state._inventory_map(raw_inventory, role=role)  # noqa: SLF001 - adversarial journal parser contract


@pytest.mark.parametrize(
    ("mutation", "message"),
    [
        (lambda payload: payload.pop("phase"), "incomplete or has unsupported fields"),
        (lambda payload: payload.update(format_version=2), "Unsupported restore journal format"),
        (lambda payload: payload.update(transaction_id="invalid"), "invalid transaction id"),
        (lambda payload: payload.update(phase="unknown"), "invalid phase"),
        (lambda payload: payload.update(staging_dir="../staging"), "invalid staging directory"),
        (lambda payload: payload.update(quarantine_dir="../quarantine"), "invalid quarantine directory"),
    ],
)
def test_restore_journal_rejects_invalid_protocol_fields(tmp_path, mutation, message) -> None:
    state_dir = tmp_path / "state"
    state_dir.mkdir()
    payload = _journal_payload()
    mutation(payload)
    state._write_json(state_dir / ".restore-journal.json", payload)  # noqa: SLF001 - malformed journal fixture

    with pytest.raises(state.StateSnapshotError, match=message):
        state.recover_interrupted_restore(state_dir)


def test_restore_journal_and_state_evidence_reject_symlinks(tmp_path) -> None:
    state_dir = tmp_path / "state"
    state_dir.mkdir()
    target = tmp_path / "target"
    target.write_bytes(b"evidence")
    (state_dir / ".restore-journal.json").symlink_to(target)
    with pytest.raises(state.StateSnapshotError, match="journal must be a regular file"):
        state.recover_interrupted_restore(state_dir)

    (state_dir / ".restore-journal.json").unlink()
    (state_dir / ".restore-journal.json").symlink_to(tmp_path / "missing-journal")
    with pytest.raises(state.StateSnapshotError, match="journal must be a regular file"):
        state.recover_interrupted_restore(state_dir)

    (state_dir / ".restore-journal.json").unlink()
    state_file = state_dir / "linked.db"
    state_file.symlink_to(target)
    with pytest.raises(state.StateSnapshotError, match="State evidence must be a regular file"):
        state._state_files(state_dir)  # noqa: SLF001 - evidence parser contract
    evidence = {"filename": "linked.db", "sha256": state._sha256(target), "size_bytes": target.stat().st_size}  # noqa: SLF001
    assert not state._matches_evidence(state_file, evidence)  # noqa: SLF001 - evidence parser contract
    with pytest.raises(state.StateSnapshotError, match="cannot verify test evidence"):
        state._require_evidence(state_file, evidence, location="test")  # noqa: SLF001 - evidence parser contract


def test_restore_recovery_rejects_orphan_and_unowned_residue(tmp_path) -> None:
    orphan_state = tmp_path / "orphan"
    orphan_state.mkdir()
    (orphan_state / ".restore-staged.orphan").mkdir()
    with pytest.raises(state.StateSnapshotError, match="residue exists without a durable journal"):
        state.recover_interrupted_restore(orphan_state)

    owned_state = tmp_path / "owned"
    owned_state.mkdir()
    payload = _journal_payload(phase="committed")
    new_database = owned_state / "new.db"
    new_database.write_bytes(b"")
    payload["new_inventory"] = [state._file_evidence(new_database)]  # noqa: SLF001 - valid journal fixture
    (owned_state / payload["staging_dir"]).mkdir()
    (owned_state / payload["quarantine_dir"]).mkdir()
    state._write_restore_journal(owned_state, payload)  # noqa: SLF001 - valid journal fixture
    (owned_state / ".restore-staged.unowned").mkdir()

    with pytest.raises(state.StateSnapshotError, match="journal does not own residue"):
        state.recover_interrupted_restore(owned_state)


@pytest.mark.parametrize("residue_role", ["staging_dir", "quarantine_dir"])
def test_committed_restore_recovery_rejects_dangling_residue_symlinks(tmp_path, residue_role: str) -> None:
    state_dir = tmp_path / "state"
    state_dir.mkdir()
    payload = _journal_payload(phase="committed")
    new_database = state_dir / "new.db"
    new_database.write_bytes(b"new generation")
    payload["new_inventory"] = [state._file_evidence(new_database)]  # noqa: SLF001 - valid journal fixture
    for role in ("staging_dir", "quarantine_dir"):
        residue = state_dir / payload[role]
        if role == residue_role:
            residue.symlink_to(tmp_path / f"missing-{role}", target_is_directory=True)
        else:
            residue.mkdir()
    state._write_restore_journal(state_dir, payload)  # noqa: SLF001 - valid journal fixture

    for _attempt in range(2):
        with pytest.raises(state.StateSnapshotError, match="evidence must be a directory"):
            state.recover_interrupted_restore(state_dir)
        assert (state_dir / ".restore-journal.json").is_file()


def test_restore_evidence_validators_fail_closed_on_missing_or_unexplained_files(tmp_path) -> None:
    state_dir = tmp_path / "state"
    state_dir.mkdir()
    known = state_dir / "known.db"
    known.write_bytes(b"known")
    evidence = {"known.db": state._file_evidence(known)}  # noqa: SLF001 - evidence validator fixture

    with pytest.raises(state.StateSnapshotError, match="missing its required directory"):
        state._validate_residue(  # noqa: SLF001 - evidence validator contract
            state_dir / "missing",
            evidence,
            role="required",
            required=True,
        )
    assert (
        state._validate_residue(  # noqa: SLF001 - evidence validator contract
            state_dir / "missing",
            evidence,
            role="optional",
            required=False,
        )
        == {}
    )

    residue = state_dir / "residue"
    residue.mkdir()
    (residue / "unexpected.db").write_bytes(b"unexpected")
    with pytest.raises(state.StateSnapshotError, match="contains unexplained evidence"):
        state._validate_residue(residue, evidence, role="test", required=True)  # noqa: SLF001

    with pytest.raises(state.StateSnapshotError, match="outside both recorded generations"):
        state._validate_live_subset(state_dir, {})  # noqa: SLF001 - live inventory contract
    mismatched = {"known.db": {**evidence["known.db"], "sha256": "0" * 64}}
    with pytest.raises(state.StateSnapshotError, match="cannot match live evidence"):
        state._validate_live_subset(state_dir, mismatched)  # noqa: SLF001 - live inventory contract
    with pytest.raises(state.StateSnapshotError, match="generation inventory mismatch"):
        state._assert_exact_live_inventory(state_dir, {}, generation="test")  # noqa: SLF001


def test_restore_recovery_initializes_clean_state_lock_and_rejects_non_directory(tmp_path) -> None:
    state.recover_interrupted_restore(tmp_path / "missing")
    clean = tmp_path / "clean"
    clean.mkdir()
    state.recover_interrupted_restore(clean)
    assert (clean / ".state-restore.lock").is_file()
    not_directory = tmp_path / "file"
    not_directory.write_bytes(b"not a directory")
    with pytest.raises(state.StateSnapshotError, match="State directory not found"):
        state.recover_interrupted_restore(not_directory)


def test_state_lock_reuses_a_read_only_regular_inode_and_rejects_unsafe_paths(monkeypatch, tmp_path) -> None:
    lock = tmp_path / ".state-restore.lock"
    lock.touch(mode=0o600)
    observed_flags: list[int] = []
    original_open = os.open

    def record_open(path, flags, mode=0o777):
        if Path(path) == lock:
            observed_flags.append(flags)
        return original_open(path, flags, mode)

    monkeypatch.setattr(os, "open", record_open)
    with state._restore_lock(lock):  # noqa: SLF001 - one-inode lock contract
        pass
    assert observed_flags[0] & os.O_ACCMODE == os.O_RDONLY

    lock.unlink()
    target = tmp_path / "target"
    target.touch()
    lock.symlink_to(target)
    with (
        pytest.raises(state.StateSnapshotError, match="must not be a symlink"),
        state._restore_lock(lock),  # noqa: SLF001 - hostile lock fixture
    ):
        pass

    lock.unlink()
    lock.mkdir()
    with (
        pytest.raises(state.StateSnapshotError, match="must be a regular file"),
        state._restore_lock(lock),  # noqa: SLF001 - hostile lock fixture
    ):
        pass


def test_backup_fails_closed_when_a_read_only_state_mount_has_no_initialized_lock(monkeypatch, tmp_path) -> None:
    _seed_state()
    lock = settings.state_dir / ".state-restore.lock"
    original_open = os.open

    def reject_lock_creation(path, flags, mode=0o777):
        if Path(path) == lock and flags & os.O_CREAT:
            raise OSError(errno.EROFS, "read-only file system", str(path))
        return original_open(path, flags, mode)

    monkeypatch.setattr(os, "open", reject_lock_creation)
    with pytest.raises(OSError, match="read-only file system"):
        state.backup_state(settings.state_dir, tmp_path / "backups")
    assert not lock.exists()
    assert not (tmp_path / "backups").exists()


def test_backup_rejects_invalid_inputs_before_publication(tmp_path) -> None:
    backup_root = tmp_path / "backups"

    with pytest.raises(state.StateSnapshotError, match="retention must be positive"):
        state.backup_state(tmp_path / "missing", backup_root, keep=0)
    with pytest.raises(state.StateSnapshotError, match="State directory not found"):
        state.backup_state(tmp_path / "missing", backup_root)

    empty_state = tmp_path / "empty"
    empty_state.mkdir()
    with pytest.raises(state.StateSnapshotError, match="No SQLite databases found"):
        state.backup_state(empty_state, backup_root)

    assert not backup_root.exists()


def test_backup_rejects_invalid_timestamp_without_publication_artifacts(monkeypatch, tmp_path) -> None:
    _seed_state()
    backup_root = tmp_path / "backups"
    monkeypatch.setenv("STATE_BACKUP_TIMESTAMP", "31-07-2026")

    with pytest.raises(state.StateSnapshotError, match="must use YYYYMMDDTHHMMSSZ"):
        state.backup_state(settings.state_dir, backup_root)

    assert backup_root.is_dir()
    assert not list(backup_root.iterdir())


def test_backup_rejects_duplicate_snapshot_and_concurrent_owner(monkeypatch, tmp_path) -> None:
    _seed_state()
    backup_root = tmp_path / "backups"
    stamp = "20990101T000000Z"
    monkeypatch.setenv("STATE_BACKUP_TIMESTAMP", stamp)
    state.backup_state(settings.state_dir, backup_root)

    with pytest.raises(state.StateSnapshotError, match="Completed snapshot already exists"):
        state.backup_state(settings.state_dir, backup_root)

    shutil.rmtree(backup_root / stamp)
    (backup_root / f".lock-{stamp}").mkdir()
    with pytest.raises(state.StateSnapshotError, match="Another backup owns timestamp"):
        state.backup_state(settings.state_dir, backup_root)


def test_backup_prunes_only_expired_complete_snapshots(monkeypatch, tmp_path) -> None:
    _seed_state()
    backup_root = tmp_path / "backups"
    monkeypatch.setenv("STATE_BACKUP_TIMESTAMP", "20990101T000000Z")
    first = state.backup_state(settings.state_dir, backup_root, keep=1)
    incomplete = backup_root / ".incomplete-manual"
    incomplete.mkdir()
    monkeypatch.setenv("STATE_BACKUP_TIMESTAMP", "20990101T000001Z")
    second = state.backup_state(settings.state_dir, backup_root, keep=1)

    assert not first.exists()
    assert second.is_dir()
    assert incomplete.is_dir()


def test_backup_wraps_sqlite_copy_failures(tmp_path) -> None:
    with pytest.raises(state.StateSnapshotError, match="Could not back up SQLite database"):
        state._copy_database(tmp_path / "missing.db", tmp_path / "copy.db")  # noqa: SLF001


def test_database_inspection_distinguishes_integrity_failure_from_open_failure(monkeypatch, tmp_path) -> None:
    class _CorruptConnection:
        def execute(self, _statement: str):
            return self

        def fetchone(self) -> tuple[str]:
            return ("database disk image is malformed",)

        def close(self) -> None:
            return

    monkeypatch.setattr(state.sqlite3, "connect", lambda *_args, **_kwargs: _CorruptConnection())
    with pytest.raises(state.StateSnapshotError, match=r"integrity check failed.*malformed"):
        state._inspect_database(tmp_path / "corrupt.db")  # noqa: SLF001

    monkeypatch.undo()
    corrupt = tmp_path / "not-sqlite.db"
    corrupt.write_bytes(b"not a SQLite database")
    with pytest.raises(state.StateSnapshotError, match="Could not inspect SQLite database"):
        state._inspect_database(corrupt)  # noqa: SLF001


def test_application_version_has_an_uninstalled_source_checkout_fallback(monkeypatch) -> None:
    def missing_distribution(_distribution: str) -> str:
        raise PackageNotFoundError

    monkeypatch.setattr(state, "version", missing_distribution)
    assert state._application_version() == "uninstalled"  # noqa: SLF001


def test_restore_publishes_exact_inventory_and_removes_absent_live_database(monkeypatch, tmp_path) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    expected = _fingerprints(snapshot)
    shutil.rmtree(settings.state_dir)
    settings.state_dir.mkdir()
    with closing(sqlite3.connect(settings.state_dir / "obsolete.db")) as connection:
        connection.execute("CREATE TABLE obsolete (value TEXT)")
        connection.commit()

    restored = state.restore_state(snapshot, settings.state_dir)

    assert [path.name for path in restored] == ["incidents.db", "runtime.db"]
    assert _fingerprints(settings.state_dir) == expected
    assert not list(settings.state_dir.glob(".restore-*"))


def test_restore_removes_wal_shm_and_journal_sidecars_from_the_previous_generation(monkeypatch, tmp_path) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    sidecars = [settings.state_dir / f"incidents.db{suffix}" for suffix in ("-wal", "-shm", "-journal")]
    for sidecar in sidecars:
        sidecar.write_bytes(b"stale sidecar")

    state.restore_state(snapshot, settings.state_dir)

    assert all(not sidecar.exists() for sidecar in sidecars)


def test_restore_rejects_bad_hash_before_mutating_live_state(monkeypatch, tmp_path) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    before = _fingerprints(settings.state_dir)
    with (snapshot / "runtime.db").open("ab") as stream:
        stream.write(b"tampered")

    with pytest.raises(state.StateSnapshotError, match="hash or size mismatch"):
        state.restore_state(snapshot, settings.state_dir)

    assert _fingerprints(settings.state_dir) == before
    assert not list(settings.state_dir.glob(".restore-*"))


def test_restore_rejects_missing_hidden_and_incomplete_snapshots_before_mutation(tmp_path) -> None:
    before = _fingerprints(_seed_state())
    missing = tmp_path / "missing"
    with pytest.raises(state.StateSnapshotError, match="Snapshot directory not found"):
        state.restore_state(missing, settings.state_dir)

    hidden = tmp_path / ".hidden"
    hidden.mkdir()
    with pytest.raises(state.StateSnapshotError, match="hidden and unpublished"):
        state.restore_state(hidden, settings.state_dir)

    incomplete = tmp_path / "incomplete"
    incomplete.mkdir()
    with pytest.raises(state.StateSnapshotError, match="Snapshot is incomplete"):
        state.restore_state(incomplete, settings.state_dir)

    assert _fingerprints(settings.state_dir) == before


@pytest.mark.parametrize(
    ("metadata_name", "contents", "message"),
    [
        ("manifest.json", "{", "Could not read snapshot metadata"),
        ("manifest.json", "[]", "must be a JSON object"),
    ],
)
def test_restore_rejects_unreadable_or_non_object_metadata(
    monkeypatch,
    tmp_path,
    metadata_name,
    contents,
    message,
) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    (snapshot / metadata_name).write_text(contents, encoding="utf-8")
    before = _fingerprints(settings.state_dir)

    with pytest.raises(state.StateSnapshotError, match=message):
        state.restore_state(snapshot, settings.state_dir)

    assert _fingerprints(settings.state_dir) == before


@pytest.mark.parametrize(
    ("corruption", "message"),
    [
        ("manifest-format", "Unsupported snapshot manifest format"),
        ("timestamp", "manifest has no creation timestamp"),
        ("source", "manifest has incomplete source identity"),
        ("inventory-empty", f"manifest has no database {_INVENTORY_TERM}"),
        ("entry-not-object", "database entry must be an object"),
        ("unsafe-name", "Unsafe snapshot database filename"),
        ("duplicate-name", "Duplicate snapshot database filename"),
        ("digest", "Invalid SHA-256"),
        ("size", "Invalid size"),
        ("identity-missing", "Missing SQLite schema identity"),
        ("inventory-files", f"{_INVENTORY_TERM} differs from its files"),
        ("schema-identity", "schema identity mismatch"),
    ],
)
def test_restore_rejects_malformed_signed_manifest_fields_before_mutation(
    monkeypatch,
    tmp_path,
    corruption,
    message,
) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    manifest = _read_manifest(snapshot)
    entry = manifest["databases"][0]
    if corruption == "manifest-format":
        manifest["format_version"] = state.SNAPSHOT_FORMAT_VERSION + 1
    elif corruption == "timestamp":
        manifest["created_at"] = ""
    elif corruption == "source":
        manifest["source"] = {"application": "agentops-agent", "version": "0.5.0"}
    elif corruption == "inventory-empty":
        manifest["databases"] = []
    elif corruption == "entry-not-object":
        manifest["databases"][0] = "incidents.db"
    elif corruption == "unsafe-name":
        entry["filename"] = "../incidents.db"
    elif corruption == "duplicate-name":
        manifest["databases"].append(dict(entry))
    elif corruption == "digest":
        entry["sha256"] = "not-a-sha256"
    elif corruption == "size":
        entry["size_bytes"] = 0
    elif corruption == "identity-missing":
        entry["sqlite"] = None
    elif corruption == "inventory-files":
        manifest["databases"].pop()
    elif corruption == "schema-identity":
        entry["sqlite"]["user_version"] += 1
    _publish_manifest(snapshot, manifest)
    before = _fingerprints(settings.state_dir)

    with pytest.raises(state.StateSnapshotError, match=message):
        state.restore_state(snapshot, settings.state_dir)

    assert _fingerprints(settings.state_dir) == before


def test_restore_rejects_manifest_hash_mismatch_before_mutation(monkeypatch, tmp_path) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    manifest_path = snapshot / "manifest.json"
    manifest_path.write_text(manifest_path.read_text(encoding="utf-8") + " ", encoding="utf-8")
    before = _fingerprints(settings.state_dir)

    with pytest.raises(state.StateSnapshotError, match="manifest hash does not match"):
        state.restore_state(snapshot, settings.state_dir)

    assert _fingerprints(settings.state_dir) == before


def test_restore_rolls_back_the_complete_generation_after_second_publish_failure(monkeypatch, tmp_path) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    with closing(sqlite3.connect(settings.state_dir / "obsolete.db")) as connection:
        connection.execute("CREATE TABLE obsolete (value TEXT)")
        connection.commit()
    before = _fingerprints(settings.state_dir)
    monkeypatch.setenv("STATE_RESTORE_FAIL_AFTER", "1")

    with pytest.raises(OSError, match="injected restore publication failure"):
        state.restore_state(snapshot, settings.state_dir)

    assert _fingerprints(settings.state_dir) == before
    assert not list(settings.state_dir.glob(".restore-*"))


def test_restore_rolls_back_the_complete_generation_when_publication_is_interrupted(
    monkeypatch,
    tmp_path,
) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    before = _fingerprints(settings.state_dir)
    original_replace = Path.replace
    staged_replaces = 0

    def interrupt_second_staged_replace(source: Path, target: Path) -> Path:
        nonlocal staged_replaces
        if source.parent.name.startswith(".restore-staged."):
            staged_replaces += 1
            if staged_replaces == 2:
                raise KeyboardInterrupt
        return original_replace(source, target)

    monkeypatch.setattr(Path, "replace", interrupt_second_staged_replace)
    with pytest.raises(KeyboardInterrupt):
        state.restore_state(snapshot, settings.state_dir)

    assert _fingerprints(settings.state_dir) == before
    assert not list(settings.state_dir.glob(".restore-*"))


def test_restore_recovers_byte_identical_old_generation_after_process_exit_on_first_replacement(
    monkeypatch,
    tmp_path,
) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    with closing(sqlite3.connect(settings.state_dir / "runtime.db")) as connection:
        connection.execute("INSERT INTO sessions VALUES ('old-after-snapshot')")
        connection.commit()
    with closing(sqlite3.connect(settings.state_dir / "obsolete.db")) as connection:
        connection.execute("CREATE TABLE obsolete (value TEXT)")
        connection.execute("INSERT INTO obsolete VALUES ('old generation')")
        connection.commit()
    (settings.state_dir / "incidents.db-journal").write_bytes(b"old sidecar evidence")
    before = _generation_fingerprints(settings.state_dir)
    child = _run_restore_child(
        """
import os
import sys
from pathlib import Path
from agent import state

snapshot = Path(sys.argv[1])
state_dir = Path(sys.argv[2])
original_replace = Path.replace

def crash_after_first_old_replacement(source, target):
    result = original_replace(source, target)
    if target.parent.name.startswith(".restore-quarantine."):
        os._exit(71)
    return result

Path.replace = crash_after_first_old_replacement
state.restore_state(snapshot, state_dir)
""",
        snapshot,
        settings.state_dir,
    )

    assert child.returncode == 71, child.stderr
    assert (settings.state_dir / ".restore-journal.json").is_file()

    state.recover_interrupted_restore(settings.state_dir)

    assert _generation_fingerprints(settings.state_dir) == before
    assert not state._restore_artifacts(settings.state_dir)  # noqa: SLF001 - recovery residue contract
    assert not (settings.state_dir / ".restore-journal.json").exists()


def test_restore_preserves_committed_new_generation_after_process_exit_before_cleanup(
    monkeypatch,
    tmp_path,
) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    expected = _fingerprints(snapshot)
    with closing(sqlite3.connect(settings.state_dir / "runtime.db")) as connection:
        connection.execute("INSERT INTO sessions VALUES ('old-after-snapshot')")
        connection.commit()
    child = _run_restore_child(
        """
import os
import sys
from pathlib import Path
from agent import state

snapshot = Path(sys.argv[1])
state_dir = Path(sys.argv[2])

def crash_before_cleanup(_state_dir, _journal):
    os._exit(72)

state._cleanup_restore_transaction = crash_before_cleanup
state.restore_state(snapshot, state_dir)
""",
        snapshot,
        settings.state_dir,
    )

    assert child.returncode == 72, child.stderr
    journal = json.loads((settings.state_dir / ".restore-journal.json").read_text(encoding="utf-8"))
    assert journal["phase"] == "committed"

    recovery_backups = tmp_path / "recovery-backups"
    monkeypatch.setenv("STATE_BACKUP_TIMESTAMP", "20990101T000001Z")
    state.main(
        [
            "backup",
            "--state-dir",
            str(settings.state_dir),
            "--backup-root",
            str(recovery_backups),
        ]
    )

    assert _fingerprints(settings.state_dir) == expected
    assert _fingerprints(recovery_backups / "20990101T000001Z") == expected
    assert not state._restore_artifacts(settings.state_dir)  # noqa: SLF001 - recovery residue contract
    assert not (settings.state_dir / ".restore-journal.json").exists()


def test_restore_adopts_a_durable_initial_journal_and_recovers_after_process_exit(
    monkeypatch,
    tmp_path,
) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    with closing(sqlite3.connect(settings.state_dir / "runtime.db")) as connection:
        connection.execute("INSERT INTO sessions VALUES ('old-after-snapshot')")
        connection.commit()
    before = _generation_fingerprints(settings.state_dir)
    child = _run_restore_child(
        """
import json
import os
import sys
from pathlib import Path
from agent import state

snapshot = Path(sys.argv[1])
state_dir = Path(sys.argv[2])
original_replace = Path.replace

def crash_after_initial_journal_fsync(source, target):
    if source.name.startswith(".restore-journal.") and target.name == ".restore-journal.json":
        candidate = json.loads(source.read_text(encoding="utf-8"))
        if candidate["phase"] == "prepared":
            os._exit(76)
    return original_replace(source, target)

Path.replace = crash_after_initial_journal_fsync
state.restore_state(snapshot, state_dir)
""",
        snapshot,
        settings.state_dir,
    )

    assert child.returncode == 76, child.stderr
    assert not (settings.state_dir / ".restore-journal.json").exists()
    assert len(list(settings.state_dir.glob(".restore-journal.*.tmp"))) == 1

    state.recover_interrupted_restore(settings.state_dir)

    assert _generation_fingerprints(settings.state_dir) == before
    assert not state._restore_artifacts(settings.state_dir)  # noqa: SLF001 - recovery residue contract
    assert not (settings.state_dir / ".restore-journal.json").exists()


@pytest.mark.parametrize(
    ("interrupted_phase", "fail_after", "exit_code"),
    [
        ("committed", None, 74),
        ("rolling_back", "1", 75),
    ],
)
def test_restore_discards_an_unrenamed_phase_update_and_recovers_the_old_generation(
    monkeypatch,
    tmp_path,
    interrupted_phase: str,
    fail_after: str | None,
    exit_code: int,
) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    with closing(sqlite3.connect(settings.state_dir / "runtime.db")) as connection:
        connection.execute("INSERT INTO sessions VALUES ('old-after-snapshot')")
        connection.commit()
    before = _generation_fingerprints(settings.state_dir)
    child = _run_restore_child(
        f"""
import json
import os
import sys
from pathlib import Path
from agent import state

snapshot = Path(sys.argv[1])
state_dir = Path(sys.argv[2])
interrupted_phase = {interrupted_phase!r}
fail_after = {fail_after!r}
exit_code = {exit_code}
original_replace = Path.replace

def crash_after_phase_temporary_fsync(source, target):
    if source.name.startswith(".restore-journal.") and target.name == ".restore-journal.json":
        candidate = json.loads(source.read_text(encoding="utf-8"))
        if candidate["phase"] == interrupted_phase:
            os._exit(exit_code)
    return original_replace(source, target)

if fail_after is not None:
    os.environ["STATE_RESTORE_FAIL_AFTER"] = fail_after
Path.replace = crash_after_phase_temporary_fsync
state.restore_state(snapshot, state_dir)
""",
        snapshot,
        settings.state_dir,
    )

    assert child.returncode == exit_code, child.stderr
    durable_journal = json.loads((settings.state_dir / ".restore-journal.json").read_text(encoding="utf-8"))
    assert durable_journal["phase"] == "prepared"
    assert len(list(settings.state_dir.glob(".restore-journal.*.tmp"))) == 1

    state.recover_interrupted_restore(settings.state_dir)

    assert _generation_fingerprints(settings.state_dir) == before
    assert not state._restore_artifacts(settings.state_dir)  # noqa: SLF001 - recovery residue contract
    assert not (settings.state_dir / ".restore-journal.json").exists()


def test_restore_recovery_fails_closed_when_old_generation_evidence_is_missing(
    monkeypatch,
    tmp_path,
) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    child = _run_restore_child(
        """
import os
import sys
from pathlib import Path
from agent import state

snapshot = Path(sys.argv[1])
state_dir = Path(sys.argv[2])
original_replace = Path.replace

def crash_after_first_old_replacement(source, target):
    result = original_replace(source, target)
    if target.parent.name.startswith(".restore-quarantine."):
        os._exit(73)
    return result

Path.replace = crash_after_first_old_replacement
state.restore_state(snapshot, state_dir)
""",
        snapshot,
        settings.state_dir,
    )
    assert child.returncode == 73, child.stderr
    journal = json.loads((settings.state_dir / ".restore-journal.json").read_text(encoding="utf-8"))
    quarantine = settings.state_dir / journal["quarantine_dir"]
    next(quarantine.iterdir()).unlink()

    with pytest.raises(state.StateSnapshotError, match="missing byte-identical old evidence"):
        state.recover_interrupted_restore(settings.state_dir)

    assert (settings.state_dir / ".restore-journal.json").is_file()


def test_concurrent_restores_are_serialized_across_processes(monkeypatch, tmp_path) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    first_locked = tmp_path / "first-locked"
    release_first = tmp_path / "release-first"
    second_locked = tmp_path / "second-locked"
    first_code = """
import sys
import time
from pathlib import Path
from agent import state

snapshot = Path(sys.argv[1])
state_dir = Path(sys.argv[2])
locked = Path(sys.argv[3])
release = Path(sys.argv[4])
original_recover = state._recover_restore_locked

def hold_lock(path):
    original_recover(path)
    locked.write_text("locked", encoding="utf-8")
    while not release.exists():
        time.sleep(0.01)

state._recover_restore_locked = hold_lock
state.restore_state(snapshot, state_dir)
"""
    second_code = """
import sys
from pathlib import Path
from agent import state

snapshot = Path(sys.argv[1])
state_dir = Path(sys.argv[2])
locked = Path(sys.argv[3])
original_recover = state._recover_restore_locked

def observe_lock(path):
    locked.write_text("locked", encoding="utf-8")
    original_recover(path)

state._recover_restore_locked = observe_lock
state.restore_state(snapshot, state_dir)
"""
    first = subprocess.Popen(  # noqa: S603 - fixed interpreter and test-owned arguments
        [
            sys.executable,
            "-c",
            first_code,
            str(snapshot),
            str(settings.state_dir),
            str(first_locked),
            str(release_first),
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        env=_child_environment(),
    )
    try:
        _wait_for_file(first_locked)
        second = subprocess.Popen(  # noqa: S603 - fixed interpreter and test-owned arguments
            [
                sys.executable,
                "-c",
                second_code,
                str(snapshot),
                str(settings.state_dir),
                str(second_locked),
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            env=_child_environment(),
        )
        try:
            time.sleep(0.2)
            assert second.poll() is None
            assert not second_locked.exists()
            release_first.write_text("continue", encoding="utf-8")
            first_stdout, first_stderr = first.communicate(timeout=10)
            second_stdout, second_stderr = second.communicate(timeout=10)
        finally:
            if second.poll() is None:
                second.kill()
                second.communicate()
    finally:
        if first.poll() is None:
            first.kill()
            first.communicate()

    assert first.returncode == 0, (first_stdout, first_stderr)
    assert second.returncode == 0, (second_stdout, second_stderr)
    assert second_locked.is_file()
    assert _fingerprints(settings.state_dir) == _fingerprints(snapshot)


def test_restore_detects_snapshot_change_during_staging_and_preserves_live_state(monkeypatch, tmp_path) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    before = _fingerprints(settings.state_dir)
    original_copyfile = shutil.copyfile

    def tampering_copyfile(source: Path, target: Path) -> Path:
        copied = original_copyfile(source, target)
        if target.parent.name.startswith(".restore-staged."):
            with target.open("ab") as stream:
                stream.write(b"changed during copy")
        return copied

    monkeypatch.setattr(shutil, "copyfile", tampering_copyfile)
    with pytest.raises(state.StateSnapshotError, match="changed while copying"):
        state.restore_state(snapshot, settings.state_dir)

    assert _fingerprints(settings.state_dir) == before
    assert not list(settings.state_dir.glob(".restore-*"))


def test_restore_reports_every_incomplete_rollback_operation(monkeypatch, tmp_path) -> None:
    state_dir = tmp_path / "state"
    quarantine = tmp_path / "quarantine"
    state_dir.mkdir()
    quarantine.mkdir()
    installed = state_dir / "new.db"
    installed.write_bytes(b"new")
    previous = quarantine / "previous.db"
    previous.write_bytes(b"previous")
    original_unlink = Path.unlink
    original_replace = Path.replace

    def fail_installed_removal(path: Path, *, missing_ok: bool = False) -> None:
        if path == installed:
            raise OSError("read-only filesystem")
        original_unlink(path, missing_ok=missing_ok)

    def fail_previous_restore(path: Path, target: Path) -> Path:
        if path == previous:
            raise OSError("device unavailable")
        return original_replace(path, target)

    monkeypatch.setattr(Path, "unlink", fail_installed_removal)
    monkeypatch.setattr(Path, "replace", fail_previous_restore)
    with pytest.raises(state.StateSnapshotError, match=r"remove new\.db.*restore previous\.db"):
        state._restore_quarantine(quarantine, state_dir, [installed])  # noqa: SLF001

    assert installed.read_bytes() == b"new"
    assert previous.read_bytes() == b"previous"


def test_backup_rejects_future_audit_schema_without_mutating_input(tmp_path) -> None:
    _seed_state()
    incident_database = settings.state_dir / "incidents.db"
    with closing(sqlite3.connect(incident_database)) as connection:
        connection.execute(
            """
            INSERT INTO audit_log
                (schema_version, ts, actor, approved_by, rationale, context_summary,
                 session_id, invocation_id, action, target, detail)
            VALUES (?, '2026-07-31T08:00:00Z', 'future-agent', 'engineer',
                    'future approval', 'future context', 'future-session',
                    'future-invocation', 'restart_service', 'inventory', 'future detail')
            """,
            (CURRENT_AUDIT_SCHEMA_VERSION + 1,),
        )
        connection.commit()
    before = incident_database.read_bytes()

    with pytest.raises(state.StateSnapshotError, match="Upgrade the application or select a compatible snapshot"):
        state.backup_state(settings.state_dir, tmp_path / "backups")

    assert incident_database.read_bytes() == before
    assert not list((tmp_path / "backups").glob("*"))


def test_backup_rejects_invalid_audit_schema_type_without_mutating_input(tmp_path) -> None:
    _seed_state()
    incident_database = settings.state_dir / "incidents.db"
    with closing(sqlite3.connect(incident_database)) as connection:
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
            (REFERENCE_DOMAIN.services.inventory,),
        )
        connection.commit()
    before = incident_database.read_bytes()

    with pytest.raises(state.StateSnapshotError, match="unsupported audit schema version"):
        state.backup_state(settings.state_dir, tmp_path / "backups")

    assert incident_database.read_bytes() == before
    assert not list((tmp_path / "backups").glob("*"))


def test_backup_rejects_future_runtime_schema_without_mutating_input(tmp_path) -> None:
    _seed_state()
    runtime_database = settings.state_dir / "runtime.db"
    with closing(sqlite3.connect(runtime_database)) as connection:
        connection.execute(
            """
            CREATE TABLE adk_internal_metadata (
                "key" TEXT PRIMARY KEY,
                value TEXT NOT NULL
            )
            """
        )
        connection.execute("INSERT INTO adk_internal_metadata VALUES ('schema_version', '99')")
        connection.commit()
    before = runtime_database.read_bytes()

    with pytest.raises(state.StateSnapshotError, match="runtime schema version '99'"):
        state.backup_state(settings.state_dir, tmp_path / "backups")

    assert runtime_database.read_bytes() == before
    assert not list((tmp_path / "backups").glob("*"))


def test_restore_rejects_a_manifested_future_schema_before_mutating_live_state(monkeypatch, tmp_path) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    snapshot_database = snapshot / "incidents.db"
    with closing(sqlite3.connect(snapshot_database)) as connection:
        connection.execute(
            """
            INSERT INTO audit_log
                (schema_version, ts, actor, approved_by, rationale, context_summary,
                 session_id, invocation_id, action, target, detail)
            VALUES (?, '2099-01-01T00:00:00Z', 'future-agent', 'engineer',
                    'future approval', 'future context', 'future-session',
                    'future-invocation', 'restart_service', ?, 'future detail')
            """,
            (
                CURRENT_AUDIT_SCHEMA_VERSION + 1,
                REFERENCE_DOMAIN.services.checkout,
            ),
        )
        connection.commit()
        identity = state._schema_identity(connection)  # noqa: SLF001 - signed future fixture
    manifest_path = snapshot / "manifest.json"
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    incident_entry = next(entry for entry in manifest["databases"] if entry["filename"] == "incidents.db")
    incident_entry.update(
        {
            "sha256": state._sha256(snapshot_database),  # noqa: SLF001
            "size_bytes": snapshot_database.stat().st_size,
            "sqlite": identity,
        }
    )
    state._write_json(manifest_path, manifest)  # noqa: SLF001
    state._write_json(  # noqa: SLF001
        snapshot / ".complete",
        {
            "format_version": state.SNAPSHOT_FORMAT_VERSION,
            "manifest_sha256": state._sha256(manifest_path),  # noqa: SLF001
        },
    )
    before = _fingerprints(settings.state_dir)

    with pytest.raises(state.StateSnapshotError, match="Upgrade the application or select a compatible snapshot"):
        state.restore_state(snapshot, settings.state_dir)

    assert _fingerprints(settings.state_dir) == before


def test_restore_rejects_a_manifested_invalid_schema_type_before_mutating_live_state(monkeypatch, tmp_path) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    snapshot_database = snapshot / "incidents.db"
    with closing(sqlite3.connect(snapshot_database)) as connection:
        connection.execute("PRAGMA ignore_check_constraints = ON")
        connection.execute(
            """
            INSERT INTO audit_log
                (schema_version, ts, actor, approved_by, rationale, context_summary,
                 session_id, invocation_id, action, target, detail)
            VALUES ('future', '2099-01-01T00:00:00Z', 'future-agent', 'engineer',
                    'invalid schema fixture', 'future context', 'invalid-session',
                    'invalid-invocation', 'restart_service', ?, 'future detail')
            """,
            (REFERENCE_DOMAIN.services.checkout,),
        )
        connection.commit()
        identity = state._schema_identity(connection)  # noqa: SLF001 - signed invalid fixture
    manifest_path = snapshot / "manifest.json"
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    incident_entry = next(entry for entry in manifest["databases"] if entry["filename"] == "incidents.db")
    incident_entry.update(
        {
            "sha256": state._sha256(snapshot_database),  # noqa: SLF001
            "size_bytes": snapshot_database.stat().st_size,
            "sqlite": identity,
        }
    )
    state._write_json(manifest_path, manifest)  # noqa: SLF001
    state._write_json(  # noqa: SLF001
        snapshot / ".complete",
        {
            "format_version": state.SNAPSHOT_FORMAT_VERSION,
            "manifest_sha256": state._sha256(manifest_path),  # noqa: SLF001
        },
    )
    before = _fingerprints(settings.state_dir)

    with pytest.raises(state.StateSnapshotError, match="unsupported audit schema version"):
        state.restore_state(snapshot, settings.state_dir)

    assert _fingerprints(settings.state_dir) == before


def test_restore_rejects_unsupported_snapshot_format_before_mutation(monkeypatch, tmp_path) -> None:
    snapshot = _snapshot(tmp_path, monkeypatch)
    marker_path = snapshot / ".complete"
    marker = json.loads(marker_path.read_text(encoding="utf-8"))
    marker["format_version"] = state.SNAPSHOT_FORMAT_VERSION + 1
    state._write_json(marker_path, marker)  # noqa: SLF001 - corrupt compatibility fixture
    before = _fingerprints(settings.state_dir)

    with pytest.raises(state.StateSnapshotError, match="Unsupported snapshot marker format"):
        state.restore_state(snapshot, settings.state_dir)

    assert _fingerprints(settings.state_dir) == before


def test_state_cli_routes_backup_restore_and_errors(monkeypatch, tmp_path) -> None:
    _seed_state()
    backup_root = tmp_path / "backups"
    stamp = "20990101T000000Z"
    monkeypatch.setenv("STATE_BACKUP_TIMESTAMP", stamp)
    state.main(
        [
            "backup",
            "--state-dir",
            str(settings.state_dir),
            "--backup-root",
            str(backup_root),
            "--keep",
            "1",
        ]
    )
    expected = _fingerprints(backup_root / stamp)
    shutil.rmtree(settings.state_dir)

    state.main(["restore", str(backup_root / stamp), "--state-dir", str(settings.state_dir)])

    assert _fingerprints(settings.state_dir) == expected
    with pytest.raises(SystemExit, match="error: Snapshot retention must be positive"):
        state.main(
            [
                "backup",
                "--state-dir",
                str(settings.state_dir),
                "--backup-root",
                str(backup_root),
                "--keep",
                "0",
            ]
        )
