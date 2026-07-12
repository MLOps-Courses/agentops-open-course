"""Validated domain models at the SQLite and tool boundaries."""

from __future__ import annotations

import re
from enum import StrEnum
from typing import Annotated

from pydantic import BaseModel, ConfigDict, Field

_INCIDENT_ID = re.compile(r"^INC-\d+$")
_SLUG = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)*$")


class IncidentStatus(StrEnum):
    """Lifecycle states stored for an incident."""

    OPEN = "open"
    INVESTIGATING = "investigating"
    RESOLVED = "resolved"


class Severity(StrEnum):
    """Incident severity, ordered by its numeric suffix."""

    SEV1 = "SEV1"
    SEV2 = "SEV2"
    SEV3 = "SEV3"


class ServiceStatus(StrEnum):
    """Health states stored for a service."""

    OPERATIONAL = "operational"
    DEGRADED = "degraded"
    DOWN = "down"


class Service(BaseModel):
    """One service parsed from the trusted dataset."""

    model_config = ConfigDict(extra="forbid")

    name: str = Field(pattern=_SLUG.pattern)
    description: str = Field(min_length=1)
    status: ServiceStatus
    owner: str = Field(min_length=1)


class Incident(BaseModel):
    """One incident parsed from the trusted dataset."""

    model_config = ConfigDict(extra="forbid")

    id: str = Field(pattern=_INCIDENT_ID.pattern)
    service: str = Field(pattern=_SLUG.pattern)
    title: str = Field(min_length=1)
    severity: Severity
    status: IncidentStatus
    runbook: str = Field(pattern=_SLUG.pattern)
    opened_at: str = Field(min_length=1)
    resolved_at: str | None
    summary: str = Field(min_length=1)


class AuditEntry(BaseModel):
    """A persisted record of one confirmed state-changing action.

    An auditor reading the append-only trail sees who approved (``approved_by``),
    why (``rationale``), and what context the human was shown (``context_summary``).
    """

    model_config = ConfigDict(extra="forbid")

    id: int
    ts: str
    actor: str
    approved_by: str
    rationale: str
    context_summary: str
    session_id: str
    invocation_id: str
    action: str
    target: str
    detail: str


class TriageReport(BaseModel):
    """A machine-consumable triage summary the model must fill (Chapter 4.0).

    This is the agent's *answer as a schema*: downstream automation (tickets,
    dashboards) consumes it directly. ``extra="forbid"`` plus field constraints
    make schema violations loud — never a silently-wrong object.
    """

    model_config = ConfigDict(extra="forbid")

    incident_id: str = Field(pattern=_INCIDENT_ID.pattern, description="The triaged incident, e.g. INC-002.")
    severity: Severity = Field(description="Severity copied from the incident record.")
    affected_services: list[Annotated[str, Field(pattern=_SLUG.pattern)]] = Field(
        min_length=1, description="Impacted service slugs, root cause first."
    )
    hypothesis: str = Field(min_length=1, description="Likely root cause in one short paragraph.")
    evidence: list[str] = Field(min_length=1, description="Log lines or tool observations backing the hypothesis.")
    recommended_runbook: str = Field(pattern=_SLUG.pattern, description="Slug of the runbook to follow.")
    proposed_actions: list[str] = Field(
        default_factory=list,
        description="Ordered next steps; guarded actions (restart/resolve) still need human approval.",
    )


def normalize_incident_id(value: str) -> str | None:
    """Normalize a model-supplied incident id, returning ``None`` when invalid."""
    normalized = value.strip().upper()
    return normalized if _INCIDENT_ID.fullmatch(normalized) else None


def normalize_slug(value: str) -> str | None:
    """Normalize a service/runbook slug, returning ``None`` when invalid."""
    normalized = value.strip().lower()
    return normalized if _SLUG.fullmatch(normalized) else None
