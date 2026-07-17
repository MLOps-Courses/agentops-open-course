"""Function tools for the AgentOps Agent (Chapter 3.1).

Each tool is a plain, typed function with a Google-style docstring — ADK reads the
signature and docstring to build the schema the model sees, then auto-wraps it as a
``FunctionTool`` when it is passed to ``Agent(tools=[...])``. Tools return plain dicts so
the result serializes cleanly back to the model.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from google.adk.agents.llm_agent import ToolUnion

from . import data
from .models import IncidentStatus, normalize_incident_id, normalize_slug
from .resilience import with_resilience


def list_incidents(status: str = "", service: str = "") -> dict[str, Any]:
    """List incidents on the platform, most recent first.

    Args:
        status: Optional filter — one of ``open``, ``investigating``, or ``resolved``.
            Leave empty to list incidents of every status.
        service: Optional service name to filter by (e.g. ``checkout``). Leave empty for all.

    Returns:
        A dict with ``count`` and an ``incidents`` list (id, service, title, severity, status).
    """
    normalized_status: IncidentStatus | None = None
    if status:
        try:
            normalized_status = IncidentStatus(status.strip().lower())
        except ValueError:
            allowed = ", ".join(item.value for item in IncidentStatus)
            return {"error": f"Invalid incident status {status!r}. Expected one of: {allowed}."}
    normalized_service = normalize_slug(service) if service else None
    if service and normalized_service is None:
        return {"error": f"Invalid service name {service!r}; expected lowercase kebab-case."}
    rows = data.list_incidents(status=normalized_status, service=normalized_service)
    incidents = [
        {
            "id": row.id,
            "service": row.service,
            "title": row.title,
            "severity": row.severity.value,
            "status": row.status.value,
            "opened_at": row.opened_at,
            "summary": row.summary,
        }
        for row in rows
    ]
    return {"count": len(incidents), "incidents": incidents}


def get_incident(incident_id: str) -> dict[str, Any]:
    """Get the full details of one incident by its id.

    Args:
        incident_id: The incident identifier, e.g. ``INC-001``.

    Returns:
        ``{"incident": {...}}`` with the record (including ``runbook`` and ``summary``),
        or an ``error`` if unknown.
    """
    normalized = normalize_incident_id(incident_id)
    if normalized is None:
        return {"error": f"Invalid incident id {incident_id!r}; expected an id like INC-002."}
    incident = data.get_incident(normalized)
    if incident is None:
        return {"error": f"No incident found with id {normalized!r}."}
    return {"incident": incident.model_dump(mode="json")}


def get_service_status(name: str) -> dict[str, Any]:
    """Get the current status of a service and its open incidents.

    Args:
        name: The service name, e.g. ``checkout`` or ``inventory``.

    Returns:
        ``{"service": {...}, "open_incidents": [...]}``, or an ``error`` if unknown.
    """
    normalized = normalize_slug(name)
    if normalized is None:
        return {"error": f"Invalid service name {name!r}; expected lowercase kebab-case."}
    service = data.get_service(normalized)
    if service is None:
        known = ", ".join(row.name for row in data.list_services())
        return {"error": f"No service named {normalized!r}. Known services: {known}."}
    open_incidents = [
        {"id": row.id, "title": row.title, "severity": row.severity.value, "status": row.status.value}
        for row in data.list_incidents(service=normalized)
        if row.status is not IncidentStatus.RESOLVED
    ]
    return {"service": service.model_dump(mode="json"), "open_incidents": open_incidents}


def search_service_logs(service: str, query: str = "", limit: int = 20) -> dict[str, Any]:
    """Search deterministic sample logs for one service, newest matching lines first.

    Args:
        service: Service slug, e.g. ``checkout`` or ``inventory``.
        query: Optional case-insensitive text that each returned line must contain.
        limit: Maximum lines to return, from 1 to 100.

    Returns:
        A dict with the normalized service, match count, and matching log lines; or an error.
    """
    normalized = normalize_slug(service)
    if normalized is None:
        return {"error": f"Invalid service name {service!r}; expected lowercase kebab-case."}
    if not 1 <= limit <= 100:
        return {"error": "Log search limit must be between 1 and 100."}
    lines = data.read_service_logs(normalized)
    if lines is None:
        known = ", ".join(path.stem for path in data.logs_dir().glob("*.log"))
        return {"error": f"No logs for service {normalized!r}. Available logs: {known}."}
    needle = query.strip().lower()
    matches = [line for line in reversed(lines) if not needle or needle in line.lower()][:limit]
    return {"service": normalized, "count": len(matches), "lines": matches}


# The tools registered on the AgentOps Agent, each wrapped with a deadline and bounded
# retries because reads are idempotent (Ch. 4.5). Guarded actions (restart/resolve)
# join in Ch. 4.5 and stay unwrapped: retrying a write could apply it twice.
ALL_TOOLS: list[ToolUnion] = [
    with_resilience(list_incidents),
    with_resilience(get_incident),
    with_resilience(get_service_status),
    with_resilience(search_service_logs),
]
