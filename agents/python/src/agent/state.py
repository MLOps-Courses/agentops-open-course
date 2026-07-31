"""Versioned, integrity-checked snapshots for the agent's SQLite state.

The host scripts and Kubernetes CronJob both call this module so snapshot
format, compatibility checks, and restore rollback have one implementation.
Writers must be stopped before restore; online backup uses SQLite's backup API.
"""

from __future__ import annotations

import argparse
import errno
import fcntl
import hashlib
import json
import logging
import os
import shutil
import sqlite3
import stat
import tempfile
import uuid
from collections.abc import Iterable, Iterator, Sequence
from contextlib import closing, contextmanager, suppress
from datetime import UTC, datetime
from importlib.metadata import PackageNotFoundError, version
from pathlib import Path
from typing import Any, Final

from .config import settings
from .models import CURRENT_AUDIT_SCHEMA_VERSION, CURRENT_RUNTIME_SCHEMA_VERSION

SNAPSHOT_FORMAT_VERSION: Final = 1
_MANIFEST_NAME = "manifest.json"
_COMPLETE_NAME = ".complete"
_RESTORE_JOURNAL_FORMAT_VERSION: Final = 1
_RESTORE_JOURNAL_NAME = ".restore-journal.json"
_RESTORE_LOCK_NAME = ".state-restore.lock"
_LOGGER = logging.getLogger(__name__)


class StateSnapshotError(RuntimeError):
    """A snapshot could not be created or safely restored."""


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _schema_identity(connection: sqlite3.Connection) -> dict[str, int | str | None]:
    rows = connection.execute(
        """
        SELECT type, name, tbl_name, COALESCE(sql, '')
        FROM sqlite_schema
        WHERE name NOT LIKE 'sqlite_%'
        ORDER BY type, name
        """
    ).fetchall()
    encoded = json.dumps(rows, separators=(",", ":"), ensure_ascii=True).encode()
    audit_version: int | None = None
    audit_columns = {row[1] for row in connection.execute("PRAGMA table_info('audit_log')")}
    if "schema_version" in audit_columns:
        maximum = connection.execute("SELECT MAX(schema_version) FROM audit_log").fetchone()
        audit_version = maximum[0] if maximum is not None else None
    runtime_version: str | None = None
    metadata_columns = {row[1] for row in connection.execute("PRAGMA table_info('adk_internal_metadata')")}
    if {"key", "value"} <= metadata_columns:
        row = connection.execute(
            """
            SELECT value
            FROM adk_internal_metadata
            WHERE "key" = 'schema_version'
            """
        ).fetchone()
        runtime_version = row[0] if row is not None else None
    return {
        "schema_sha256": hashlib.sha256(encoded).hexdigest(),
        "user_version": connection.execute("PRAGMA user_version").fetchone()[0],
        "audit_schema_version": audit_version,
        "runtime_schema_version": runtime_version,
    }


def _reject_unsupported_audit_schema(connection: sqlite3.Connection, filename: str) -> None:
    """Reject audit versions this application cannot parse or migrate."""
    audit_columns = {row[1] for row in connection.execute("PRAGMA table_info('audit_log')")}
    if "schema_version" not in audit_columns:
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
        raise StateSnapshotError(
            f"{filename} contains unsupported audit schema version {unsupported[0]!r} "
            f"with SQLite type {unsupported[1]!r}; this application supports integer versions "
            f"from 1 through {CURRENT_AUDIT_SCHEMA_VERSION}. "
            "Upgrade the application or select a compatible snapshot."
        )


def _inspect_database(path: Path) -> dict[str, int | str | None]:
    try:
        with closing(sqlite3.connect(f"{path.resolve().as_uri()}?mode=ro", uri=True, timeout=5)) as connection:
            connection.execute("PRAGMA query_only = ON")
            integrity = connection.execute("PRAGMA integrity_check").fetchone()
            if integrity is None or integrity[0] != "ok":
                status = "no result" if integrity is None else str(integrity[0])
                raise StateSnapshotError(f"SQLite integrity check failed for {path.name}: {status}")
            _reject_unsupported_audit_schema(connection, path.name)
            identity = _schema_identity(connection)
    except StateSnapshotError:
        raise
    except (OSError, sqlite3.Error) as error:
        raise StateSnapshotError(f"Could not inspect SQLite database {path}") from error
    runtime_version = identity["runtime_schema_version"]
    if runtime_version is not None and runtime_version != CURRENT_RUNTIME_SCHEMA_VERSION:
        raise StateSnapshotError(
            f"{path.name} contains runtime schema version {runtime_version!r}, but this application supports "
            f"{CURRENT_RUNTIME_SCHEMA_VERSION!r}. Upgrade the application or select a compatible snapshot."
        )
    return identity


def _copy_database(source: Path, target: Path) -> None:
    try:
        with (
            closing(sqlite3.connect(f"{source.resolve().as_uri()}?mode=ro", uri=True, timeout=30)) as source_connection,
            closing(sqlite3.connect(target, timeout=30)) as target_connection,
        ):
            source_connection.execute("PRAGMA query_only = ON")
            source_connection.backup(target_connection)
    except (OSError, sqlite3.Error) as error:
        raise StateSnapshotError(f"Could not back up SQLite database {source}") from error


def _application_version() -> str:
    try:
        return version("agentops-agent")
    except PackageNotFoundError:
        return "uninstalled"


def _source_commit() -> str:
    return os.getenv("AGENT_SOURCE_COMMIT", "unknown").strip() or "unknown"


def _snapshot_stamp() -> str:
    configured = os.getenv("STATE_BACKUP_TIMESTAMP")
    stamp = configured or datetime.now(UTC).strftime("%Y%m%dT%H%M%SZ")
    try:
        datetime.strptime(stamp, "%Y%m%dT%H%M%SZ").replace(tzinfo=UTC)
    except ValueError as error:
        raise StateSnapshotError(f"STATE_BACKUP_TIMESTAMP must use YYYYMMDDTHHMMSSZ, got {stamp!r}") from error
    return stamp


def _write_json(path: Path, payload: object) -> None:
    path.write_text(
        json.dumps(payload, indent=2, sort_keys=True, ensure_ascii=True) + "\n",
        encoding="utf-8",
    )
    with path.open("rb") as stream:
        os.fsync(stream.fileno())


def _fsync_directory(path: Path) -> None:
    descriptor = os.open(path, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def _completed_snapshots(backup_root: Path) -> list[Path]:
    return sorted(
        (
            candidate
            for candidate in backup_root.iterdir()
            if candidate.is_dir() and not candidate.name.startswith(".") and (candidate / _COMPLETE_NAME).is_file()
        ),
        key=lambda candidate: candidate.name,
        reverse=True,
    )


@contextmanager
def _restore_lock(lock_path: Path) -> Iterator[None]:
    """Serialize backup, restore, and crash recovery across processes."""
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    no_follow = getattr(os, "O_NOFOLLOW", 0)

    def open_existing() -> int:
        try:
            return os.open(lock_path, os.O_RDONLY | no_follow)
        except OSError as error:
            if error.errno == errno.ELOOP:
                raise StateSnapshotError(f"State generation lock must not be a symlink: {lock_path}") from error
            raise

    try:
        descriptor = open_existing()
    except FileNotFoundError:
        try:
            descriptor = os.open(lock_path, os.O_RDWR | os.O_CREAT | os.O_EXCL | no_follow, 0o600)
        except FileExistsError:
            descriptor = open_existing()
    try:
        if not stat.S_ISREG(os.fstat(descriptor).st_mode):
            raise StateSnapshotError(f"State generation lock must be a regular file: {lock_path}")
        fcntl.flock(descriptor, fcntl.LOCK_EX)
        yield
    finally:
        fcntl.flock(descriptor, fcntl.LOCK_UN)
        os.close(descriptor)


def backup_state(
    state_dir: Path,
    backup_root: Path,
    *,
    keep: int = 7,
) -> Path:
    """Publish one complete online-safe snapshot and apply bounded retention."""
    if keep <= 0:
        raise StateSnapshotError(f"Snapshot retention must be positive, got {keep}.")
    if not state_dir.is_dir():
        raise StateSnapshotError(f"State directory not found: {state_dir}")
    with _restore_lock(state_dir / _RESTORE_LOCK_NAME):
        _recover_restore_locked(state_dir)
        return _backup_state_locked(state_dir, backup_root, keep=keep)


def _backup_state_locked(state_dir: Path, backup_root: Path, *, keep: int) -> Path:
    """Create a snapshot while restore publication is excluded."""
    databases = sorted(state_dir.glob("*.db"), key=lambda path: path.name)
    if not databases:
        raise StateSnapshotError(f"No SQLite databases found under {state_dir}")

    backup_root.mkdir(parents=True, exist_ok=True)
    stamp = _snapshot_stamp()
    snapshot = backup_root / stamp
    lock = backup_root / f".lock-{stamp}"
    if snapshot.exists():
        raise StateSnapshotError(f"Completed snapshot already exists: {snapshot}")
    try:
        lock.mkdir()
    except FileExistsError as error:
        raise StateSnapshotError(f"Another backup owns timestamp {stamp}") from error

    staging: Path | None = None
    try:
        staging = Path(tempfile.mkdtemp(dir=backup_root, prefix=f".incomplete-{stamp}."))
        inventory: list[dict[str, Any]] = []
        for database in databases:
            target = staging / database.name
            _copy_database(database, target)
            identity = _inspect_database(target)
            inventory.append(
                {
                    "filename": database.name,
                    "sha256": _sha256(target),
                    "size_bytes": target.stat().st_size,
                    "sqlite": identity,
                }
            )
            _LOGGER.info("backed up %s -> %s", database.name, target)

        manifest = {
            "format_version": SNAPSHOT_FORMAT_VERSION,
            "created_at": datetime.now(UTC).isoformat(timespec="seconds").replace("+00:00", "Z"),
            "source": {
                "application": "agentops-agent",
                "version": _application_version(),
                "commit": _source_commit(),
            },
            "databases": inventory,
        }
        manifest_path = staging / _MANIFEST_NAME
        _write_json(manifest_path, manifest)
        _write_json(
            staging / _COMPLETE_NAME,
            {
                "format_version": SNAPSHOT_FORMAT_VERSION,
                "manifest_sha256": _sha256(manifest_path),
            },
        )
        _fsync_directory(staging)
        staging.rename(snapshot)
        staging = None
        _fsync_directory(backup_root)
    finally:
        if staging is not None:
            shutil.rmtree(staging, ignore_errors=True)
        with suppress(FileNotFoundError):
            lock.rmdir()

    for expired in _completed_snapshots(backup_root)[keep:]:
        shutil.rmtree(expired)
        _LOGGER.info("pruned %s", expired)
    _LOGGER.info("snapshot complete: %s", snapshot)
    return snapshot


def _load_json_object(path: Path) -> dict[str, Any]:
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as error:
        raise StateSnapshotError(f"Could not read snapshot metadata {path}") from error
    if not isinstance(payload, dict):
        raise StateSnapshotError(f"Snapshot metadata must be a JSON object: {path}")
    return payload


def _validate_inventory(snapshot: Path) -> list[dict[str, Any]]:
    if not snapshot.is_dir():
        raise StateSnapshotError(f"Snapshot directory not found: {snapshot}")
    if snapshot.name.startswith("."):
        raise StateSnapshotError(f"Snapshot is hidden and unpublished: {snapshot}")
    marker_path = snapshot / _COMPLETE_NAME
    manifest_path = snapshot / _MANIFEST_NAME
    if not marker_path.is_file() or not manifest_path.is_file():
        raise StateSnapshotError(
            f"Snapshot is incomplete: expected {_COMPLETE_NAME} and {_MANIFEST_NAME} under {snapshot}"
        )
    marker = _load_json_object(marker_path)
    manifest = _load_json_object(manifest_path)
    if marker.get("format_version") != SNAPSHOT_FORMAT_VERSION:
        raise StateSnapshotError(
            f"Unsupported snapshot marker format {marker.get('format_version')!r}; "
            f"this application supports format {SNAPSHOT_FORMAT_VERSION}."
        )
    if manifest.get("format_version") != SNAPSHOT_FORMAT_VERSION:
        raise StateSnapshotError(
            f"Unsupported snapshot manifest format {manifest.get('format_version')!r}; "
            f"this application supports format {SNAPSHOT_FORMAT_VERSION}."
        )
    created_at = manifest.get("created_at")
    source = manifest.get("source")
    if not isinstance(created_at, str) or not created_at:
        raise StateSnapshotError(f"Snapshot manifest has no creation timestamp: {manifest_path}")
    if not isinstance(source, dict) or any(
        not isinstance(source.get(field), str) or not source[field] for field in ("application", "version", "commit")
    ):
        raise StateSnapshotError(f"Snapshot manifest has incomplete source identity: {manifest_path}")
    if marker.get("manifest_sha256") != _sha256(manifest_path):
        raise StateSnapshotError(f"Snapshot manifest hash does not match {_COMPLETE_NAME}: {snapshot}")

    raw_inventory = manifest.get("databases")
    if not isinstance(raw_inventory, list) or not raw_inventory:
        raise StateSnapshotError(f"Snapshot manifest has no database inventory: {manifest_path}")
    inventory: list[dict[str, Any]] = []
    names: set[str] = set()
    for raw_entry in raw_inventory:
        if not isinstance(raw_entry, dict):
            raise StateSnapshotError(f"Snapshot database entry must be an object: {manifest_path}")
        name = raw_entry.get("filename")
        digest = raw_entry.get("sha256")
        size = raw_entry.get("size_bytes")
        identity = raw_entry.get("sqlite")
        if not isinstance(name, str) or Path(name).name != name or not name.endswith(".db") or name.startswith("."):
            raise StateSnapshotError(f"Unsafe snapshot database filename: {name!r}")
        if name in names:
            raise StateSnapshotError(f"Duplicate snapshot database filename: {name}")
        if not isinstance(digest, str) or len(digest) != 64:
            raise StateSnapshotError(f"Invalid SHA-256 for snapshot database {name}")
        if not isinstance(size, int) or size <= 0:
            raise StateSnapshotError(f"Invalid size for snapshot database {name}")
        if not isinstance(identity, dict):
            raise StateSnapshotError(f"Missing SQLite schema identity for snapshot database {name}")
        names.add(name)
        inventory.append(raw_entry)

    actual_names = {path.name for path in snapshot.glob("*.db")}
    if actual_names != names:
        raise StateSnapshotError(
            "Snapshot database inventory differs from its files: "
            f"manifest={sorted(names)}, files={sorted(actual_names)}"
        )
    for entry in inventory:
        database = snapshot / entry["filename"]
        if database.stat().st_size != entry["size_bytes"] or _sha256(database) != entry["sha256"]:
            raise StateSnapshotError(f"Snapshot database hash or size mismatch: {database.name}")
        if _inspect_database(database) != entry["sqlite"]:
            raise StateSnapshotError(f"Snapshot database schema identity mismatch: {database.name}")
    return inventory


def _is_state_filename(name: str) -> bool:
    return name.endswith((".db", ".db-wal", ".db-shm", ".db-journal"))


def _state_files(state_dir: Path) -> list[Path]:
    files: list[Path] = []
    for path in state_dir.iterdir():
        if not _is_state_filename(path.name):
            continue
        if path.is_symlink() or not path.is_file():
            raise StateSnapshotError(f"State evidence must be a regular file: {path}")
        files.append(path)
    return sorted(files, key=lambda path: path.name)


def _file_evidence(path: Path) -> dict[str, str | int]:
    return {
        "filename": path.name,
        "sha256": _sha256(path),
        "size_bytes": path.stat().st_size,
    }


def _inventory_map(raw_inventory: object, *, role: str) -> dict[str, dict[str, str | int]]:
    if not isinstance(raw_inventory, list) or (role == "new" and not raw_inventory):
        raise StateSnapshotError(f"Restore journal has an invalid {role} inventory.")
    inventory: dict[str, dict[str, str | int]] = {}
    for raw_entry in raw_inventory:
        if not isinstance(raw_entry, dict) or set(raw_entry) != {"filename", "sha256", "size_bytes"}:
            raise StateSnapshotError(f"Restore journal has incomplete {role} file evidence.")
        name = raw_entry.get("filename")
        digest = raw_entry.get("sha256")
        size = raw_entry.get("size_bytes")
        safe_name = (
            isinstance(name, str)
            and Path(name).name == name
            and not name.startswith(".")
            and _is_state_filename(name)
            and (role == "old" or name.endswith(".db"))
        )
        if not safe_name:
            raise StateSnapshotError(f"Restore journal has an unsafe {role} filename: {name!r}")
        if name in inventory:
            raise StateSnapshotError(f"Restore journal repeats {role} filename: {name}")
        if (
            not isinstance(digest, str)
            or len(digest) != 64
            or digest != digest.lower()
            or any(character not in "0123456789abcdef" for character in digest)
        ):
            raise StateSnapshotError(f"Restore journal has an invalid {role} SHA-256 for {name}.")
        if not isinstance(size, int) or isinstance(size, bool) or size < 0:
            raise StateSnapshotError(f"Restore journal has an invalid {role} size for {name}.")
        inventory[name] = {
            "filename": name,
            "sha256": digest,
            "size_bytes": size,
        }
    return inventory


def _matches_evidence(path: Path, evidence: dict[str, str | int]) -> bool:
    if path.is_symlink() or not path.is_file():
        return False
    return path.stat().st_size == evidence["size_bytes"] and _sha256(path) == evidence["sha256"]


def _require_evidence(path: Path, evidence: dict[str, str | int], *, location: str) -> None:
    if not _matches_evidence(path, evidence):
        raise StateSnapshotError(
            f"Restore recovery cannot verify {location} evidence for {evidence['filename']}; refusing to guess."
        )


def _restore_artifacts(state_dir: Path) -> list[Path]:
    prefixes = (".restore-staged.", ".restore-quarantine.", ".restore-journal.")
    return sorted(
        (path for path in state_dir.iterdir() if path.name != _RESTORE_JOURNAL_NAME and path.name.startswith(prefixes)),
        key=lambda path: path.name,
    )


def _write_restore_journal(state_dir: Path, journal: dict[str, Any]) -> None:
    transaction_id = journal["transaction_id"]
    temporary = state_dir / f".restore-journal.{transaction_id}.tmp"
    target = state_dir / _RESTORE_JOURNAL_NAME
    encoded = (json.dumps(journal, indent=2, sort_keys=True, ensure_ascii=True) + "\n").encode()
    descriptor: int | None = None
    try:
        descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "wb") as stream:
            descriptor = None
            stream.write(encoded)
            stream.flush()
            os.fsync(stream.fileno())
        # Persist the temporary directory entry before it becomes the only
        # recoverable record of this transaction. A file fsync alone does not
        # make a newly created name durable across power loss.
        _fsync_directory(state_dir)
        temporary.replace(target)
        _fsync_directory(state_dir)
    finally:
        if descriptor is not None:
            os.close(descriptor)
        temporary.unlink(missing_ok=True)


def _read_restore_journal(path: Path) -> dict[str, Any]:
    if path.is_symlink() or not path.is_file():
        raise StateSnapshotError(f"Restore journal must be a regular file: {path}")
    journal = _load_json_object(path)
    expected_fields = {
        "format_version",
        "transaction_id",
        "phase",
        "staging_dir",
        "quarantine_dir",
        "old_inventory",
        "new_inventory",
    }
    if set(journal) != expected_fields:
        raise StateSnapshotError("Restore journal is incomplete or has unsupported fields.")
    if journal.get("format_version") != _RESTORE_JOURNAL_FORMAT_VERSION:
        raise StateSnapshotError(
            f"Unsupported restore journal format {journal.get('format_version')!r}; "
            f"this application supports format {_RESTORE_JOURNAL_FORMAT_VERSION}."
        )
    transaction_id = journal.get("transaction_id")
    if (
        not isinstance(transaction_id, str)
        or len(transaction_id) != 32
        or any(character not in "0123456789abcdef" for character in transaction_id)
    ):
        raise StateSnapshotError("Restore journal has an invalid transaction id.")
    if journal.get("phase") not in {"prepared", "rolling_back", "committed"}:
        raise StateSnapshotError(f"Restore journal has an invalid phase: {journal.get('phase')!r}")
    if journal.get("staging_dir") != f".restore-staged.{transaction_id}":
        raise StateSnapshotError("Restore journal has an invalid staging directory.")
    if journal.get("quarantine_dir") != f".restore-quarantine.{transaction_id}":
        raise StateSnapshotError("Restore journal has an invalid quarantine directory.")
    _inventory_map(journal.get("old_inventory"), role="old")
    _inventory_map(journal.get("new_inventory"), role="new")
    return journal


def _load_restore_journal(state_dir: Path) -> dict[str, Any] | None:
    path = state_dir / _RESTORE_JOURNAL_NAME
    if path.is_symlink():
        raise StateSnapshotError(f"Restore journal must be a regular file: {path}")
    if not path.exists():
        return None
    return _read_restore_journal(path)


def _journal_paths(state_dir: Path, journal: dict[str, Any]) -> tuple[Path, Path, Path]:
    staging = state_dir / journal["staging_dir"]
    quarantine = state_dir / journal["quarantine_dir"]
    temporary = state_dir / f".restore-journal.{journal['transaction_id']}.tmp"
    return staging, quarantine, temporary


def _discard_owned_journal_temporary(state_dir: Path, journal: dict[str, Any]) -> None:
    """Discard an unrenamed phase update while the durable journal remains authoritative."""
    _, _, temporary = _journal_paths(state_dir, journal)
    if temporary.is_symlink():
        raise StateSnapshotError(f"Restore journal temporary evidence must be a regular file: {temporary}")
    if not temporary.exists():
        return
    if not temporary.is_file():
        raise StateSnapshotError(f"Restore journal temporary evidence must be a regular file: {temporary}")
    temporary.unlink()
    _fsync_directory(state_dir)


def _validate_residue(
    directory: Path,
    expected: dict[str, dict[str, str | int]],
    *,
    role: str,
    required: bool,
) -> dict[str, Path]:
    if directory.is_symlink():
        raise StateSnapshotError(f"Restore {role} evidence must be a directory: {directory}")
    if not directory.exists():
        if required:
            raise StateSnapshotError(f"Restore recovery is missing its {role} directory: {directory.name}")
        return {}
    if not directory.is_dir():
        raise StateSnapshotError(f"Restore {role} evidence must be a directory: {directory}")
    actual: dict[str, Path] = {}
    for path in directory.iterdir():
        evidence = expected.get(path.name)
        if evidence is None:
            raise StateSnapshotError(f"Restore {role} contains unexplained evidence: {path.name}")
        _require_evidence(path, evidence, location=role)
        actual[path.name] = path
    return actual


def _validate_live_subset(
    state_dir: Path,
    *inventories: dict[str, dict[str, str | int]],
) -> dict[str, Path]:
    live = {path.name: path for path in _state_files(state_dir)}
    expected_names = set().union(*(set(inventory) for inventory in inventories))
    unexplained = set(live) - expected_names
    if unexplained:
        raise StateSnapshotError(
            "Restore recovery found live state outside both recorded generations: " + ", ".join(sorted(unexplained))
        )
    for name, path in live.items():
        if not any(name in inventory and _matches_evidence(path, inventory[name]) for inventory in inventories):
            raise StateSnapshotError(
                f"Restore recovery cannot match live evidence for {name} to a recorded generation."
            )
    return live


def _validate_forward_evidence(state_dir: Path, journal: dict[str, Any]) -> None:
    old = _inventory_map(journal["old_inventory"], role="old")
    new = _inventory_map(journal["new_inventory"], role="new")
    staging, quarantine, temporary = _journal_paths(state_dir, journal)
    staged = _validate_residue(staging, new, role="staging", required=True)
    quarantined = _validate_residue(quarantine, old, role="quarantine", required=True)
    live = _validate_live_subset(state_dir, old, new)
    if temporary.is_symlink() or (temporary.exists() and not temporary.is_file()):
        raise StateSnapshotError(f"Restore journal temporary evidence is invalid: {temporary}")
    for name, evidence in old.items():
        if name in quarantined:
            continue
        path = live.get(name)
        if path is None or not _matches_evidence(path, evidence):
            raise StateSnapshotError(f"Restore recovery is missing byte-identical old evidence for {name}.")
    for name, evidence in new.items():
        if name in staged:
            continue
        path = live.get(name)
        if path is None or not _matches_evidence(path, evidence):
            raise StateSnapshotError(f"Restore recovery is missing prepared new evidence for {name}.")


def _validate_rollback_evidence(state_dir: Path, journal: dict[str, Any]) -> None:
    old = _inventory_map(journal["old_inventory"], role="old")
    new = _inventory_map(journal["new_inventory"], role="new")
    staging, quarantine, temporary = _journal_paths(state_dir, journal)
    _validate_residue(staging, new, role="staging", required=False)
    quarantined = _validate_residue(quarantine, old, role="quarantine", required=False)
    live = _validate_live_subset(state_dir, old, new)
    if temporary.is_symlink() or (temporary.exists() and not temporary.is_file()):
        raise StateSnapshotError(f"Restore journal temporary evidence is invalid: {temporary}")
    for name, evidence in old.items():
        if name in quarantined:
            continue
        path = live.get(name)
        if path is None or not _matches_evidence(path, evidence):
            raise StateSnapshotError(f"Restore recovery is missing byte-identical old evidence for {name}.")


def _assert_exact_live_inventory(
    state_dir: Path,
    expected: dict[str, dict[str, str | int]],
    *,
    generation: str,
) -> None:
    live = {path.name: path for path in _state_files(state_dir)}
    if set(live) != set(expected):
        raise StateSnapshotError(
            f"Restore {generation} generation inventory mismatch: expected={sorted(expected)}, actual={sorted(live)}"
        )
    for name, evidence in expected.items():
        _require_evidence(live[name], evidence, location=f"{generation} live")


def _set_restore_phase(state_dir: Path, journal: dict[str, Any], phase: str) -> None:
    _discard_owned_journal_temporary(state_dir, journal)
    journal["phase"] = phase
    _write_restore_journal(state_dir, journal)


def _cleanup_restore_transaction(state_dir: Path, journal: dict[str, Any]) -> None:
    staging, quarantine, temporary = _journal_paths(state_dir, journal)
    for directory in (staging, quarantine):
        if directory.is_symlink():
            raise StateSnapshotError(f"Restore cleanup evidence must be a directory: {directory}")
        if directory.exists():
            if not directory.is_dir():
                raise StateSnapshotError(f"Restore cleanup evidence must be a directory: {directory}")
            shutil.rmtree(directory)
    temporary.unlink(missing_ok=True)
    _fsync_directory(state_dir)
    (state_dir / _RESTORE_JOURNAL_NAME).unlink()
    _fsync_directory(state_dir)


def _rollback_restore(state_dir: Path, journal: dict[str, Any]) -> None:
    old = _inventory_map(journal["old_inventory"], role="old")
    new = _inventory_map(journal["new_inventory"], role="new")
    _, quarantine, _ = _journal_paths(state_dir, journal)
    quarantine.mkdir(exist_ok=True)
    _fsync_directory(state_dir)

    live = {path.name: path for path in _state_files(state_dir)}
    for name, evidence in old.items():
        quarantined = quarantine / name
        if quarantined.exists():
            _require_evidence(quarantined, evidence, location="quarantine")
            continue
        current = live.get(name)
        if current is None:
            raise StateSnapshotError(f"Restore rollback lost old evidence for {name}.")
        _require_evidence(current, evidence, location="old live")
        current.replace(quarantined)
        _fsync_directory(quarantine)
        _fsync_directory(state_dir)

    for current in _state_files(state_dir):
        evidence = new.get(current.name)
        if evidence is None or not _matches_evidence(current, evidence):
            raise StateSnapshotError(
                f"Restore rollback found unexplained replacement evidence for {current.name}; refusing to remove it."
            )
        current.unlink()
        _fsync_directory(state_dir)

    for name, evidence in old.items():
        quarantined = quarantine / name
        _require_evidence(quarantined, evidence, location="quarantine")
        quarantined.replace(state_dir / name)
        _fsync_directory(quarantine)
        _fsync_directory(state_dir)
    _assert_exact_live_inventory(state_dir, old, generation="rolled-back")
    _cleanup_restore_transaction(state_dir, journal)


def _adopt_initial_restore_journal(state_dir: Path, artifacts: list[Path]) -> dict[str, Any]:
    """Adopt a fully durable initial journal only before state publication began."""
    candidates = [
        path for path in artifacts if path.name.startswith(".restore-journal.") and path.name.endswith(".tmp")
    ]
    if len(candidates) != 1:
        raise StateSnapshotError(
            "Restore residue exists without a durable journal; expected one durable initial journal: "
            + ", ".join(path.name for path in artifacts)
        )
    temporary = candidates[0]
    journal = _read_restore_journal(temporary)
    if journal["phase"] != "prepared":
        raise StateSnapshotError("An initial restore journal must record the prepared phase.")

    staging, quarantine, expected_temporary = _journal_paths(state_dir, journal)
    allowed = {staging.name, quarantine.name, expected_temporary.name}
    actual = {path.name for path in artifacts}
    if temporary != expected_temporary or actual != allowed:
        raise StateSnapshotError(
            "Initial restore journal does not own its exact residue: "
            f"expected={sorted(allowed)}, actual={sorted(actual)}"
        )

    old = _inventory_map(journal["old_inventory"], role="old")
    new = _inventory_map(journal["new_inventory"], role="new")
    staged = _validate_residue(staging, new, role="staging", required=True)
    quarantined = _validate_residue(quarantine, old, role="quarantine", required=True)
    if set(staged) != set(new) or quarantined:
        raise StateSnapshotError("Initial restore residue shows that state publication already began.")
    _assert_exact_live_inventory(state_dir, old, generation="pre-restore")

    temporary.replace(state_dir / _RESTORE_JOURNAL_NAME)
    _fsync_directory(state_dir)
    return journal


def _recover_restore_locked(state_dir: Path) -> None:
    journal = _load_restore_journal(state_dir)
    artifacts = _restore_artifacts(state_dir)
    if journal is None:
        if not artifacts:
            return
        journal = _adopt_initial_restore_journal(state_dir, artifacts)

    staging, quarantine, temporary = _journal_paths(state_dir, journal)
    allowed_artifacts = {staging.name, quarantine.name, temporary.name}
    unexplained = [path.name for path in artifacts if path.name not in allowed_artifacts]
    if unexplained:
        raise StateSnapshotError("Restore journal does not own residue: " + ", ".join(unexplained))
    _discard_owned_journal_temporary(state_dir, journal)

    phase = journal["phase"]
    if phase == "committed":
        old = _inventory_map(journal["old_inventory"], role="old")
        new = _inventory_map(journal["new_inventory"], role="new")
        _validate_residue(staging, new, role="staging", required=False)
        _validate_residue(quarantine, old, role="quarantine", required=False)
        _assert_exact_live_inventory(state_dir, new, generation="committed")
        _cleanup_restore_transaction(state_dir, journal)
        _LOGGER.info("recovered committed restore: %s", state_dir)
        return

    if phase == "prepared":
        _validate_forward_evidence(state_dir, journal)
        _set_restore_phase(state_dir, journal, "rolling_back")
    else:
        _validate_rollback_evidence(state_dir, journal)
    _rollback_restore(state_dir, journal)
    _LOGGER.info("rolled back interrupted restore: %s", state_dir)


def recover_interrupted_restore(state_dir: Path) -> None:
    """Recover a journaled restore before any process publishes or reads state."""
    if not state_dir.exists():
        return
    if not state_dir.is_dir():
        raise StateSnapshotError(f"State directory not found: {state_dir}")
    # Taking the generation lock even for a clean directory creates the one
    # shared lock inode that the read-only Kubernetes backup mount reuses.
    # Failing to initialize that inode makes backup fail closed instead of
    # silently using a different lock.
    with _restore_lock(state_dir / _RESTORE_LOCK_NAME):
        _recover_restore_locked(state_dir)


def _restore_quarantine(quarantine: Path, state_dir: Path, installed: Iterable[Path]) -> None:
    """Compatibility helper for reporting multiple immediate rollback errors."""
    rollback_errors: list[str] = []
    for path in installed:
        try:
            path.unlink(missing_ok=True)
        except OSError as error:
            rollback_errors.append(f"remove {path.name}: {error}")
    for path in sorted(quarantine.iterdir(), key=lambda item: item.name):
        try:
            path.replace(state_dir / path.name)
        except OSError as error:
            rollback_errors.append(f"restore {path.name}: {error}")
    if rollback_errors:
        raise StateSnapshotError("Restore failed and rollback was incomplete: " + "; ".join(rollback_errors))


def restore_state(
    snapshot: Path,
    state_dir: Path,
) -> list[Path]:
    """Restore one generation with durable crash recovery across every file."""
    inventory = _validate_inventory(snapshot)
    state_dir.mkdir(parents=True, exist_ok=True)
    with _restore_lock(state_dir / _RESTORE_LOCK_NAME):
        _recover_restore_locked(state_dir)
        transaction_id = uuid.uuid4().hex
        staging = state_dir / f".restore-staged.{transaction_id}"
        quarantine = state_dir / f".restore-quarantine.{transaction_id}"
        staging.mkdir()
        quarantine.mkdir()
        _fsync_directory(state_dir)
        journal: dict[str, Any] | None = None
        try:
            new_inventory: list[dict[str, str | int]] = []
            for entry in inventory:
                source = snapshot / entry["filename"]
                target = staging / entry["filename"]
                shutil.copyfile(source, target)
                with target.open("rb") as stream:
                    os.fsync(stream.fileno())
                if target.stat().st_size != entry["size_bytes"] or _sha256(target) != entry["sha256"]:
                    raise StateSnapshotError(f"Staged snapshot database changed while copying: {source.name}")
                new_inventory.append(_file_evidence(target))
            _fsync_directory(staging)

            current_files = _state_files(state_dir)
            old_inventory = [_file_evidence(path) for path in current_files]
            journal = {
                "format_version": _RESTORE_JOURNAL_FORMAT_VERSION,
                "transaction_id": transaction_id,
                "phase": "prepared",
                "staging_dir": staging.name,
                "quarantine_dir": quarantine.name,
                "old_inventory": old_inventory,
                "new_inventory": new_inventory,
            }
            _write_restore_journal(state_dir, journal)

            for current in current_files:
                current.replace(quarantine / current.name)
                _fsync_directory(quarantine)
                _fsync_directory(state_dir)

            fail_after_raw = os.getenv("STATE_RESTORE_FAIL_AFTER", "")
            fail_after = int(fail_after_raw) if fail_after_raw else None
            installed: list[Path] = []
            for staged in sorted(staging.glob("*.db"), key=lambda path: path.name):
                if fail_after is not None and len(installed) >= fail_after:
                    raise OSError(f"injected restore publication failure after {fail_after} database(s)")
                target = state_dir / staged.name
                staged.replace(target)
                installed.append(target)
                _fsync_directory(staging)
                _fsync_directory(state_dir)

            expected_new = _inventory_map(new_inventory, role="new")
            _assert_exact_live_inventory(state_dir, expected_new, generation="new")
            _set_restore_phase(state_dir, journal, "committed")
            _cleanup_restore_transaction(state_dir, journal)
        except BaseException:
            if journal is None:
                shutil.rmtree(staging, ignore_errors=True)
                shutil.rmtree(quarantine, ignore_errors=True)
                _fsync_directory(state_dir)
            elif journal["phase"] != "committed":
                if journal["phase"] == "prepared":
                    _set_restore_phase(state_dir, journal, "rolling_back")
                _validate_rollback_evidence(state_dir, journal)
                _rollback_restore(state_dir, journal)
            raise

    installed = [state_dir / entry["filename"] for entry in inventory]
    for database in installed:
        _LOGGER.info("restored %s -> %s", database.name, database)
    _LOGGER.info("restore complete: %s", state_dir)
    return installed


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    subcommands = parser.add_subparsers(dest="command", required=True)
    backup = subcommands.add_parser("backup", help="publish a versioned SQLite snapshot")
    backup.add_argument("--state-dir", type=Path, default=settings.state_dir)
    backup.add_argument("--backup-root", type=Path, default=Path(".state-backups"))
    backup.add_argument("--keep", type=int, default=int(os.getenv("STATE_BACKUP_KEEP", "7")))
    restore = subcommands.add_parser("restore", help="restore one complete snapshot")
    restore.add_argument("snapshot", type=Path)
    restore.add_argument("--state-dir", type=Path, default=settings.state_dir)
    return parser


def main(argv: Sequence[str] | None = None) -> None:
    """Run the repository-owned backup/restore command."""
    logging.basicConfig(level=logging.INFO, format="%(message)s")
    arguments = _parser().parse_args(argv)
    try:
        if arguments.command == "backup":
            backup_state(
                arguments.state_dir,
                arguments.backup_root,
                keep=arguments.keep,
            )
        else:
            restore_state(arguments.snapshot, arguments.state_dir)
    except (OSError, ValueError, StateSnapshotError) as error:
        raise SystemExit(f"error: {error}") from error


if __name__ == "__main__":
    main()
