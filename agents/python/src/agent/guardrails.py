"""Safety guardrails — a ``before_tool_callback`` that validates inputs (Chapter 4.5).

Guardrails run *before* a tool executes: they can inspect the arguments and short-circuit
the call by returning a result dict (which the model sees instead of running the tool), or
return ``None`` to let the call proceed. This one fails fast on malformed inputs to the
mutating actions — a boundary check kept separate from the actions' own business logic.
"""

from __future__ import annotations

import re
from typing import Any

from google.adk.tools.base_tool import BaseTool
from google.adk.tools.tool_context import ToolContext

_INCIDENT_ID = re.compile(r"^INC-\d+$")

# Tools that change state — the ones worth validating strictly before they run.
_ACTION_TOOLS = frozenset({"restart_service", "resolve_incident"})


def validate_actions(tool: BaseTool, args: dict[str, Any], tool_context: ToolContext) -> dict[str, Any] | None:
    """Reject malformed inputs to mutating actions before they touch state."""
    del tool_context  # part of the ADK callback signature; unused here
    if tool.name not in _ACTION_TOOLS:
        return None
    if tool.name == "resolve_incident":
        incident_id = str(args.get("incident_id", ""))
        if not _INCIDENT_ID.match(incident_id):
            return {"error": f"Refusing to resolve {incident_id!r}: expected an id like INC-002."}
    if tool.name == "restart_service" and not str(args.get("name", "")).strip():
        return {"error": "Refusing to restart: no service name was provided."}
    return None
