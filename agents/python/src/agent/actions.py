"""Guarded mock actions — the AgentOps Agent's write side (Chapter 4.5).

These tools change state (a service's status, an incident's resolution) and append to the
audit log. They are **guarded**: wrapped in a ``FunctionTool(require_confirmation=True)`` so
ADK pauses for human approval (HITL) before the function runs. Approval is attributable
change management, not a yes/no click: the confirmation payload must carry the approver's
rationale, and the same transaction that performs the action records who approved, why, and
the current decision context at execution. A client must keep the evidence that justified
the proposal and the original action arguments visible before approval. Everything is mock
and local — no real infrastructure is touched.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

from google.adk.tools.function_tool import FunctionTool
from google.adk.tools.tool_context import ToolContext

from . import data
from .models import MAX_AUDIT_RATIONALE_LENGTH, normalize_incident_id, normalize_slug
from .pii import redact_persisted_text

# Who the audit log records as the actor for agent-initiated actions.
_ACTOR = "agentops-agent"


@dataclass(frozen=True, slots=True)
class _Approval:
    """Validated approval data safe to cross into the mutation layer."""

    approved_by: str
    rationale: str
    session_id: str
    invocation_id: str


# --8<-- [start:validated-approval]
def _validated_approval(tool_context: ToolContext | None) -> _Approval | str:
    """Parse a confirmed, attributable approval or return a refusal reason.

    The human approves by answering the confirmation request with a payload like
    ``{"rationale": "why this is safe now"}`` (a bare string also works).
    The public function fails closed even when called outside ``FunctionTool``:
    confirmation, identity, session, invocation, and rationale are all required.
    """
    if tool_context is None:
        return "the action must run through an ADK confirmation flow"
    confirmation = getattr(tool_context, "tool_confirmation", None)
    if confirmation is None or getattr(confirmation, "confirmed", False) is not True:
        return "the action has not been confirmed"
    user_id = getattr(tool_context, "user_id", None)
    session = getattr(tool_context, "session", None)
    session_id = getattr(session, "id", None)
    invocation_id = getattr(tool_context, "invocation_id", None)
    identities = {
        "approver identity": user_id,
        "session id": session_id,
        "invocation id": invocation_id,
    }
    missing = [label for label, value in identities.items() if not isinstance(value, str) or not value.strip()]
    if missing:
        return f"the confirmed action is missing {', '.join(missing)}"
    # The dict-based ``missing`` check already guarantees these are non-empty strings, but
    # the type checker cannot see that through the aggregate. This guard narrows each name
    # to ``str`` for the ``_Approval`` fields below (``assert`` is disallowed by lint S101).
    if not isinstance(user_id, str) or not isinstance(session_id, str) or not isinstance(invocation_id, str):
        return "the confirmed action has invalid identity metadata"
    payload = getattr(confirmation, "payload", None)
    if isinstance(payload, str) and payload.strip():
        rationale = payload.strip()
    elif isinstance(payload, dict):
        rationale = str(payload.get("rationale", "")).strip()
    else:
        rationale = ""
    if not rationale:
        return "the approval carried no rationale"
    if len(rationale) > MAX_AUDIT_RATIONALE_LENGTH:
        return f"the approval rationale exceeds {MAX_AUDIT_RATIONALE_LENGTH} characters"
    rationale = redact_persisted_text(rationale)
    if len(rationale) > MAX_AUDIT_RATIONALE_LENGTH:
        return f"the redacted approval rationale exceeds {MAX_AUDIT_RATIONALE_LENGTH} characters"
    return _Approval(
        approved_by=user_id.strip(),
        rationale=rationale,
        session_id=session_id.strip(),
        invocation_id=invocation_id.strip(),
    )


# --8<-- [end:validated-approval]


def _approval_error(action: str, reason: str) -> dict[str, Any]:
    return {
        "error": (
            f"Refusing {action}: {reason}. Ask the approver to confirm through the runtime "
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
    approval = _validated_approval(tool_context)
    if isinstance(approval, str):
        return _approval_error(f"restart of {normalized!r}", approval)
    entry = data.restart_service_with_audit(
        normalized,
        actor=_ACTOR,
        approved_by=approval.approved_by,
        rationale=approval.rationale,
        session_id=approval.session_id,
        invocation_id=approval.invocation_id,
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
    approval = _validated_approval(tool_context)
    if isinstance(approval, str):
        return _approval_error(f"resolution of {normalized!r}", approval)
    entry = data.resolve_incident_with_audit(
        normalized,
        actor=_ACTOR,
        approved_by=approval.approved_by,
        rationale=approval.rationale,
        session_id=approval.session_id,
        invocation_id=approval.invocation_id,
    )
    if entry is None:
        return {"error": f"Incident {normalized!r} is already resolved."}
    return {"result": f"Incident {normalized!r} marked resolved.", "audit": entry.model_dump(mode="json")}


# Guarded actions: ADK requests human approval before the function runs (HITL).
ACTION_TOOLS = [
    FunctionTool(func=restart_service, require_confirmation=True),
    FunctionTool(func=resolve_incident, require_confirmation=True),
]
