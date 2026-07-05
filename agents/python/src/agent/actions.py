"""Guarded mock actions — the Ops Copilot's write side (Chapter 4.5).

These tools change state (a service's status, an incident's resolution) and append to the
audit log. They are **guarded**: wrapped in a ``FunctionTool(require_confirmation=True)`` so
ADK pauses for human approval (HITL) before the function runs. Everything is mock and local —
no real infrastructure is touched.
"""

from __future__ import annotations

from typing import Any

from google.adk.tools.function_tool import FunctionTool

from . import data

# Who the audit log records as the actor for agent-initiated actions.
_ACTOR = "ops-copilot"


def restart_service(name: str) -> dict[str, Any]:
    """Restart a service (mock) — flips it back to operational and writes an audit entry.

    Args:
        name: The service to restart, e.g. ``inventory``.

    Returns:
        A dict describing the outcome, or an ``error`` if the service is unknown.
    """
    if data.get_service(name) is None:
        return {"error": f"No service named {name!r}; nothing to restart."}
    data.set_service_status(name, "operational")
    entry = data.append_audit(_ACTOR, "restart_service", name, "service restarted (mock)")
    return {"result": f"Service {name!r} restarted and marked operational.", "audit": entry}


def resolve_incident(incident_id: str) -> dict[str, Any]:
    """Resolve an incident (mock) — marks it resolved and writes an audit entry.

    Args:
        incident_id: The incident to resolve, e.g. ``INC-002``.

    Returns:
        A dict describing the outcome, or an ``error`` if the incident is unknown or already resolved.
    """
    if data.get_incident(incident_id) is None:
        return {"error": f"No incident with id {incident_id!r}."}
    if not data.resolve_incident(incident_id):
        return {"error": f"Incident {incident_id!r} is already resolved."}
    entry = data.append_audit(_ACTOR, "resolve_incident", incident_id, "incident resolved (mock)")
    return {"result": f"Incident {incident_id!r} marked resolved.", "audit": entry}


# Guarded actions: ADK requests human approval before the function runs (HITL).
ACTION_TOOLS = [
    FunctionTool(func=restart_service, require_confirmation=True),
    FunctionTool(func=resolve_incident, require_confirmation=True),
]
