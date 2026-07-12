"""A deterministic triage → diagnose → recommend workflow (Chapter 3.5).

Where ``root_agent`` (an LlmAgent) decides its own steps, a ``Workflow`` runs a fixed graph:
agents are nodes, and an edge chains them so each runs in order, passing findings forward via
session state. ``Workflow`` is ADK 2.0's graph runtime — it supersedes the classic
``SequentialAgent`` / ``ParallelAgent`` / ``LoopAgent`` (now deprecated) and also expresses
parallel, looping, and dynamic DAGs.
"""

from __future__ import annotations

from google.adk import Agent, Workflow
from google.adk.agents.llm_agent import ToolUnion

from .guardrails import handle_model_error, handle_tool_error, secure_tool_output
from .memory import KNOWLEDGE_TOOLS
from .model import build_model
from .pii import redact_request_pii, redact_response_pii
from .tools import ALL_TOOLS

# Each step is a focused agent; the Workflow chains them. Tools reflect what each step needs.
_DIAGNOSE_TOOLS: list[ToolUnion] = [*ALL_TOOLS, *KNOWLEDGE_TOOLS]

# 1) Triage: find the incident that matters most right now.
triage = Agent(
    model=build_model(),
    name="triage",
    description="Finds the most urgent unresolved incident.",
    instruction=(
        "You triage incidents. List the unresolved incidents and pick the single most urgent one "
        "(lowest SEV number wins). State its id, service, severity, and one-line summary."
    ),
    tools=ALL_TOOLS,
    before_model_callback=redact_request_pii,
    after_model_callback=redact_response_pii,
    after_tool_callback=secure_tool_output,
    on_model_error_callback=handle_model_error,
    on_tool_error_callback=handle_tool_error,
)

# 2) Diagnose: explain the likely cause of the triaged incident.
diagnose = Agent(
    model=build_model(),
    name="diagnose",
    description="Explains the likely cause of the triaged incident.",
    instruction=(
        "You diagnose the incident chosen by triage. Use get_incident for its details and its "
        "runbook (get_runbook), and get_service_status for the service. Explain the likely cause "
        "in two or three sentences, citing the runbook."
    ),
    tools=_DIAGNOSE_TOOLS,
    before_model_callback=redact_request_pii,
    after_model_callback=redact_response_pii,
    after_tool_callback=secure_tool_output,
    on_model_error_callback=handle_model_error,
    on_tool_error_callback=handle_tool_error,
)

# 3) Recommend: propose concrete, runbook-backed next steps.
recommend = Agent(
    model=build_model(),
    name="recommend",
    description="Recommends concrete, runbook-backed remediation.",
    instruction=(
        "You recommend remediation for the diagnosed incident. Using the runbook, give a short, "
        "ordered list of next steps. Flag any step that needs a guarded action (restart_service, "
        "resolve_incident) and requires human approval. Cite the runbook you used."
    ),
    tools=KNOWLEDGE_TOOLS,
    before_model_callback=redact_request_pii,
    after_model_callback=redact_response_pii,
    after_tool_callback=secure_tool_output,
    on_model_error_callback=handle_model_error,
    on_tool_error_callback=handle_tool_error,
)

# The graph: START → triage → diagnose → recommend, sharing session state along the way.
triage_workflow = Workflow(
    name="triage_workflow",
    description="Runs triage → diagnose → recommend over the current incidents.",
    edges=[("START", triage, diagnose, recommend)],
)
