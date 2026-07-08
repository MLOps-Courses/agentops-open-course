"""Multi-agent delegation — a coordinator with a diagnosis sub-agent (Chapter 3.6).

Sub-agents are how one agent hands work to another: the coordinator triages, then *delegates*
a deep root-cause analysis to a specialist by transferring control (ADK routes to the named
sub-agent, which shares the session). The same wiring underpins the A2A protocol — expose the
specialist over A2A (``adk api_server``) and the coordinator can call
it across the network as a ``RemoteA2aAgent`` instead of an in-process sub-agent.
"""

from __future__ import annotations

from google.adk import Agent

from .config import settings
from .memory import KNOWLEDGE_TOOLS
from .tools import ALL_TOOLS

# The specialist: given an incident, works out the likely cause from its runbook.
diagnosis_agent = Agent(
    model=settings.model,
    name="diagnosis_agent",
    description="Specialist that diagnoses a specific incident using its runbook and service status.",
    instruction=(
        "You are a diagnosis specialist. Given an incident id, use get_incident for its details and "
        "runbook, get_runbook for the runbook body, and get_service_status for the service. Explain "
        "the likely root cause in a few sentences and cite the runbook."
    ),
    tools=[*ALL_TOOLS, *KNOWLEDGE_TOOLS],
)

# The coordinator: triages, and delegates deep diagnosis to the specialist sub-agent.
coordinator_agent = Agent(
    model=settings.model,
    name="coordinator_agent",
    description="On-call coordinator that triages incidents and delegates diagnosis.",
    instruction=(
        "You are the on-call coordinator. Triage with list_incidents and get_service_status. When a "
        "specific incident needs a root-cause analysis, delegate to the diagnosis_agent sub-agent, "
        "then summarize its findings for the engineer."
    ),
    tools=ALL_TOOLS,
    sub_agents=[diagnosis_agent],
)
