"""The Ops Copilot — the AgentOps Open Course reference agent (Python track).

An on-call assistant that helps engineers triage and resolve incidents for a fictional
platform, using a 100% local, bundled dataset. It grows chapter by chapter: tools (3.1),
skills (3.2), MCP (3.3), memory/RAG (3.4), workflows (3.5), and A2A delegation (3.6).
"""

from __future__ import annotations

from google.adk import Agent

from .actions import ACTION_TOOLS
from .config import settings
from .guardrails import validate_actions
from .memory import KNOWLEDGE_TOOLS
from .tools import ALL_TOOLS

# The persona and operating rules. Kept explicit so behavior is reproducible and evaluable.
INSTRUCTION = """\
You are the Ops Copilot, an on-call assistant for a fictional online platform.
You help engineers triage and resolve incidents quickly and safely.

Operating rules:
- Always ground your answers in the tools. Never invent incidents, services, or statuses.
- When asked about incidents or a service, call the matching tool and report exactly what it returns.
- To recommend a fix, consult the runbooks: an incident carries a `runbook` slug — fetch it with
  `get_runbook`, or use `search_runbooks` to find guidance by symptom. Cite the runbook you used.
- Taking an action (restart_service, resolve_incident) changes state and needs human approval —
  propose it, and only call the tool when the engineer asks you to. Report the audit result.
- Refer to incidents by id (e.g. INC-001) and services by name (e.g. checkout).
- Be concise and actionable: lead with the answer, then the key details.
- If a tool returns an error or no data, say so plainly instead of guessing.
"""

root_agent = Agent(
    model=settings.model,
    name="agentops_agent",
    description="An on-call Ops Copilot that triages and resolves incidents from a local dataset.",
    instruction=INSTRUCTION,
    tools=[*ALL_TOOLS, *KNOWLEDGE_TOOLS, *ACTION_TOOLS],
    before_tool_callback=validate_actions,
)
