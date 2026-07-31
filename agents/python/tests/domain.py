"""Test-only dataset adapter built on the production-owned domain vocabulary."""

from __future__ import annotations

import sqlite3
from collections import Counter
from contextlib import closing
from pathlib import Path

from agent.domain import (
    REFERENCE_DOMAIN as REFERENCE_DOMAIN,
)
from agent.domain import (
    DomainVocabulary,
    IncidentVocabulary,
    RunbookVocabulary,
    ServiceVocabulary,
)

PIVOT_DOMAIN = DomainVocabulary(
    incidents=IncidentVocabulary(
        checkout_latency="INC-101",
        inventory_down="INC-102",
        payments_errors="INC-103",
        auth_errors="INC-104",
        search_latency="INC-105",
        checkout_disk="INC-106",
        cache_memory="INC-107",
        database_cascade="INC-108",
        checkout_cascade="INC-109",
        gateway_memory="INC-110",
    ),
    services=ServiceVocabulary(
        checkout="orders",
        payments="billing",
        auth="identity",
        search="catalog-search",
        inventory="catalog",
        database="datastore",
        cache="redis",
        gateway="edge",
    ),
    runbooks=RunbookVocabulary(
        cascade_failure="dependency-cascade",
        deployment_rollback="release-rollback",
        disk_full="storage-full",
        elevated_errors="error-spike",
        high_latency="slow-requests",
        memory_leak="heap-growth",
        service_down="service-unavailable",
    ),
    dependency_edges=(
        ("redis", "datastore"),
        ("datastore", "orders"),
        ("catalog", "orders"),
    ),
)


def _mapping(source: DomainVocabulary, target: DomainVocabulary) -> dict[str, str]:
    source_values = (*source.incidents.values(), *source.services.values(), *source.runbooks.values())
    target_values = (*target.incidents.values(), *target.services.values(), *target.runbooks.values())
    for label, values in (("source", source_values), ("target", target_values)):
        collisions = sorted(value for value, count in Counter(values).items() if count > 1)
        if collisions:
            rendered = ", ".join(collisions)
            raise ValueError(f"{label} domain vocabulary contains duplicate values: {rendered}")
    pairs = zip(source_values, target_values, strict=True)
    return dict(pairs)


def adapt_dataset(data_dir: Path, source: DomainVocabulary, target: DomainVocabulary) -> None:
    """Pivot the copied seed, logs, runbooks, and runtime skills through one seam."""
    replacements = _mapping(source, target)
    database = data_dir / "incidents.db"
    with closing(sqlite3.connect(database)) as connection:
        for old, new in zip(source.incidents.values(), target.incidents.values(), strict=True):
            connection.execute("UPDATE incidents SET id = ? WHERE id = ?", (new, old))
        for old, new in zip(source.services.values(), target.services.values(), strict=True):
            connection.execute("UPDATE services SET name = ? WHERE name = ?", (new, old))
            connection.execute("UPDATE incidents SET service = ? WHERE service = ?", (new, old))
        for old, new in zip(source.runbooks.values(), target.runbooks.values(), strict=True):
            connection.execute("UPDATE incidents SET runbook = ? WHERE runbook = ?", (new, old))
        for old, new in replacements.items():
            connection.execute(
                """
                UPDATE incidents
                SET title = replace(title, ?, ?),
                    summary = replace(summary, ?, ?)
                """,
                (old, new, old, new),
            )
            connection.execute(
                "UPDATE services SET description = replace(description, ?, ?)",
                (old, new),
            )
        connection.commit()

    for path in sorted(data_dir.rglob("*")):
        if not path.is_file() or path.suffix not in {".json", ".log", ".md", ".txt", ".yaml", ".yml"}:
            continue
        content = path.read_text(encoding="utf-8")
        for old, new in replacements.items():
            content = content.replace(old, new)
        path.write_text(content, encoding="utf-8")

    for old, new in zip(source.services.values(), target.services.values(), strict=True):
        old_log = data_dir / "logs" / f"{old}.log"
        if old_log.exists():
            old_log.replace(old_log.with_name(f"{new}.log"))
    for old, new in zip(source.runbooks.values(), target.runbooks.values(), strict=True):
        old_runbook = data_dir / "runbooks" / f"{old}.md"
        if old_runbook.exists():
            old_runbook.replace(old_runbook.with_name(f"{new}.md"))
