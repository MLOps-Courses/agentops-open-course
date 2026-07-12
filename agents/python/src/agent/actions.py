"""Guarded mock actions — the Ops Copilot's write side (Chapter 4.5).

These tools change state (a service's status, an incident's resolution) and append to the
audit log. They are **guarded**: wrapped in a ``FunctionTool(require_confirmation=True)`` so
ADK pauses for human approval (HITL) before the function runs. Approval is attributable
change management, not a yes/no click: the confirmation payload must carry the approver's
rationale, and the same transaction that performs the action records who approved, why, and
the decision context they were shown. Everything is mock and local — no real infrastructure
is touched.
"""

from __future__ import annotations

from typing import Any

from google.adk.tools.function_tool import FunctionTool
from google.adk.tools.tool_context import ToolContext

from . import data
from .models import IncidentStatus, normalize_incident_id, normalize_slug

# Who the audit log records as the actor for agent-initiated actions.
_ACTOR = "ops-copilot"

# Identity recorded for programmatic invocations outside an ADK approval flow
# (scripts, tests). These bypass HITL by construction and are labeled as such.
_DIRECT_CALL = "direct-call"


def _audit_identity(tool_context: ToolContext | None) -> tuple[str, str, str]:
    """Return approver, session, and invocation ids for the audit record."""
    if tool_context is None:
        return _DIRECT_CALL, _DIRECT_CALL, _DIRECT_CALL
    return tool_context.user_id, tool_context.session.id, tool_context.invocation_id


def _approval_rationale(tool_context: ToolContext | None) -> str | None:
    """Extract the approver's rationale from the ADK confirmation payload.

    The human approves by answering the confirmation request with a payload like
    ``{"rationale": "why this is safe now"}`` (a bare string also works). Returns
    ``None`` when the approval carries no usable rationale — the action then refuses
    to run, because an unexplained approval is not auditable change management.
    """
    if tool_context is None:
        return _DIRECT_CALL  # programmatic call: attributed as such, not silently blank
    confirmation = getattr(tool_context, "tool_confirmation", None)
    payload = getattr(confirmation, "payload", None)
    if isinstance(payload, str) and payload.strip():
        return payload.strip()
    if isinstance(payload, dict):
        rationale = str(payload.get("rationale", "")).strip()
        if rationale:
            return rationale
    return None


def _missing_rationale_error(action: str) -> dict[str, Any]:
    return {
        "error": (
            f"Refusing {action}: the approval carried no rationale. Ask the approver to confirm "
            'with a payload such as {"rationale": "why this action is appropriate now"}.'
        )
    }


def restart_service(name: str, tool_context: ToolContext | None = None) -> dict[str, Any]:
    """Restart a service (mock) — flips it back to operational and writes an audit entry.

    Args:
        name: The service to restart, e.g. ``inventory``.

    Returns:
        A dict describing the outcome, or an ``error`` if the service is unknown
        or the approval carried no rationale.
    """
    normalized = normalize_slug(name)
    if normalized is None:
        return {"error": f"Invalid service name {name!r}; expected lowercase kebab-case."}
    service = data.get_service(normalized)
    if service is None:
        return {"error": f"No service named {normalized!r}; nothing to restart."}
    rationale = _approval_rationale(tool_context)
    if rationale is None:
        return _missing_rationale_error(f"restart of {normalized!r}")
    open_incidents = [
        row.id for row in data.list_incidents(service=normalized) if row.status is not IncidentStatus.RESOLVED
    ]
    # What the approver was shown when they said yes — recorded with the action.
    context_summary = (
        f"service {normalized} was {service.status.value}; open incidents: {', '.join(open_incidents) or 'none'}"
    )
    approved_by, session_id, invocation_id = _audit_identity(tool_context)
    entry = data.restart_service_with_audit(
        normalized,
        actor=_ACTOR,
        approved_by=approved_by,
        rationale=rationale,
        context_summary=context_summary,
        session_id=session_id,
        invocation_id=invocation_id,
    )
    if entry is None:
        return {"error": f"No service named {normalized!r}; nothing to restart."}
    return {
        "result": f"Service {normalized!r} restarted and marked operational.",
        "audit": entry.model_dump(mode="json"),
    }


def resolve_incident(incident_id: str, tool_context: ToolContext | None = None) -> dict[str, Any]:
    """Resolve an incident (mock) — marks it resolved and writes an audit entry.

    Args:
        incident_id: The incident to resolve, e.g. ``INC-002``.

    Returns:
        A dict describing the outcome, or an ``error`` if the incident is unknown,
        already resolved, or the approval carried no rationale.
    """
    normalized = normalize_incident_id(incident_id)
    if normalized is None:
        return {"error": f"Invalid incident id {incident_id!r}; expected an id like INC-002."}
    incident = data.get_incident(normalized)
    if incident is None:
        return {"error": f"No incident with id {normalized!r}."}
    rationale = _approval_rationale(tool_context)
    if rationale is None:
        return _missing_rationale_error(f"resolution of {normalized!r}")
    context_summary = (
        f"incident {normalized} ({incident.severity.value}, {incident.status.value}) on {incident.service}: "
        f"{incident.title}; runbook {incident.runbook}"
    )
    approved_by, session_id, invocation_id = _audit_identity(tool_context)
    entry = data.resolve_incident_with_audit(
        normalized,
        actor=_ACTOR,
        approved_by=approved_by,
        rationale=rationale,
        context_summary=context_summary,
        session_id=session_id,
        invocation_id=invocation_id,
    )
    if entry is None:
        return {"error": f"Incident {normalized!r} is already resolved."}
    return {"result": f"Incident {normalized!r} marked resolved.", "audit": entry.model_dump(mode="json")}


# Guarded actions: ADK requests human approval before the function runs (HITL).
ACTION_TOOLS = [
    FunctionTool(func=restart_service, require_confirmation=True),
    FunctionTool(func=resolve_incident, require_confirmation=True),
]
