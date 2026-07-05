"""Function tools for the Ops Copilot (Chapter 3.1).

Each tool is a plain, typed function with a Google-style docstring — ADK reads the
signature and docstring to build the schema the model sees, then auto-wraps it as a
``FunctionTool`` when it is passed to ``Agent(tools=[...])``. Tools return plain dicts so
the result serializes cleanly back to the model.
"""

from __future__ import annotations

from typing import Any

from google.adk.agents.llm_agent import ToolUnion

from . import data


def list_incidents(status: str = "", service: str = "") -> dict[str, Any]:
    """List incidents on the platform, most recent first.

    Args:
        status: Optional filter — one of ``open``, ``investigating``, or ``resolved``.
            Leave empty to list incidents of every status.
        service: Optional service name to filter by (e.g. ``checkout``). Leave empty for all.

    Returns:
        A dict with ``count`` and an ``incidents`` list (id, service, title, severity, status).
    """
    rows = data.list_incidents(status=status or None, service=service or None)
    incidents = [
        {
            "id": row["id"],
            "service": row["service"],
            "title": row["title"],
            "severity": row["severity"],
            "status": row["status"],
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
    incident = data.get_incident(incident_id)
    if incident is None:
        return {"error": f"No incident found with id {incident_id!r}."}
    return {"incident": incident}


def get_service_status(name: str) -> dict[str, Any]:
    """Get the current status of a service and its open incidents.

    Args:
        name: The service name, e.g. ``checkout`` or ``inventory``.

    Returns:
        ``{"service": {...}, "open_incidents": [...]}``, or an ``error`` if unknown.
    """
    service = data.get_service(name)
    if service is None:
        known = ", ".join(row["name"] for row in data.list_services())
        return {"error": f"No service named {name!r}. Known services: {known}."}
    open_incidents = [
        {"id": row["id"], "title": row["title"], "severity": row["severity"], "status": row["status"]}
        for row in data.list_incidents(service=name)
        if row["status"] != "resolved"
    ]
    return {"service": service, "open_incidents": open_incidents}


# The tools registered on the Ops Copilot. Guarded actions (restart/resolve) join in Ch. 4.5.
ALL_TOOLS: list[ToolUnion] = [list_incidents, get_incident, get_service_status]
