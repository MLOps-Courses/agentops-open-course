"""A learner-owned incident assistant; this lab changes only synthetic state."""

from __future__ import annotations

from google.adk import Workflow
from google.adk.agents import Agent
from google.adk.models import BaseLlm
from google.adk.tools import ToolContext

INSTRUCTION = "Investigate fictional incidents using evidence. Ask before taking action."


def _incidents() -> list[dict[str, str]]:
    """Read the course's immutable SQLite fixture, never runtime state."""
    import sqlite3
    from contextlib import closing
    from pathlib import Path

    seed = Path(__file__).resolve()
    # Learner files live at repository-root learning/step-N/learner_agent/agent.py.
    # Reference solutions live at agents/python/labs/solutions/step_N.py.
    root = next(parent for parent in seed.parents if (parent / "agents/data/incidents.db").is_file())
    with closing(sqlite3.connect(f"file:{root / 'agents/data/incidents.db'}?mode=ro", uri=True)) as connection:
        connection.row_factory = sqlite3.Row
        return [
            dict(row) for row in connection.execute("SELECT id, service, severity, status FROM incidents ORDER BY id")
        ]


def list_open_incidents() -> list[dict[str, str]]:
    """List open incidents from the immutable course seed."""
    return [incident for incident in _incidents() if incident["status"] == "open"]


def get_incident(incident_id: str) -> dict[str, str]:
    """Look up an incident; report a missing id rather than inventing evidence."""
    return next(
        (incident for incident in _incidents() if incident["id"] == incident_id), {"error": "Incident not found"}
    )


def remember_incident(incident_id: str, tool_context: ToolContext) -> dict[str, str]:
    """Remember a real incident in this conversation only."""
    incident = get_incident(incident_id)
    if "error" in incident:
        return incident
    tool_context.state["incident_id"] = incident_id
    return {"incident_id": incident_id}


def propose_restart(service: str, tool_context: ToolContext) -> dict[str, str]:
    """Simulate a restart after human approval; never control a real service."""
    if service not in {incident["service"] for incident in _incidents()}:
        return {"error": "Unknown service"}
    confirmation = tool_context.tool_confirmation
    if confirmation is None:
        tool_context.request_confirmation(
            hint="Approve this simulated restart and provide a rationale.", payload={"rationale": ""}
        )
        return {"status": "approval-required"}
    if not confirmation.confirmed:
        return {"status": "denied"}
    payload = confirmation.payload
    reason = payload.get("rationale", "") if isinstance(payload, dict) else ""
    if not isinstance(reason, str) or not reason.strip():
        return {"error": "Approval needs a rationale"}
    key = f"restart:{tool_context.function_call_id}:{service}"
    if key not in tool_context.state:
        tool_context.state[key] = {"service": service, "rationale": reason}
    return {"status": "simulated", "service": service}


TOOLS = [list_open_incidents, get_incident, remember_incident, propose_restart]


def build_agent(model: str | BaseLlm) -> Agent:
    """Build without making a model request; the caller owns the model."""
    return Agent(name="learner_agent", model=model, instruction=INSTRUCTION, tools=TOOLS)


def build_workflow(model: str | BaseLlm) -> Workflow:
    """Investigate then recommend; neither step can restart a service."""
    investigate = Agent(
        name="investigate",
        model=model,
        instruction="Read incident evidence.",
        tools=[get_incident, list_open_incidents],
    )
    recommend = Agent(
        name="recommend",
        model=model,
        instruction="Use the preceding evidence to recommend a next step. Do not take action.",
    )
    return Workflow(name="triage_workflow", edges=[("START", investigate, recommend)])
