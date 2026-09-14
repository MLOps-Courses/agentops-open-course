"""A learner-owned incident assistant; this lab changes only synthetic state."""

from __future__ import annotations

from google.adk.agents import Agent
from google.adk.models import BaseLlm

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


TOOLS = [list_open_incidents, get_incident]


def build_agent(model: str | BaseLlm) -> Agent:
    """Build without making a model request; the caller owns the model."""
    return Agent(name="learner_agent", model=model, instruction=INSTRUCTION, tools=TOOLS)
