"""Multi-agent delegation — a coordinator with least-privilege specialists (Chapter 3.6).

Sub-agents are how one agent hands work to another: the coordinator triages, then
*delegates* by transferring control (ADK routes to the named sub-agent, which shares
the session). The security lesson is **per-agent least privilege**: the diagnosis
specialist holds only read tools (a prompt injection in a log line cannot make it
mutate state — it has no write tool to call), and the remediation specialist holds
only the guarded write actions (it cannot read raw logs, and every action still
pauses for human confirmation). The same wiring underpins A2A — expose a specialist
over A2A (``adk api_server``) and the coordinator can call it across the network as
a ``RemoteA2aAgent`` instead of an in-process sub-agent.
"""

from __future__ import annotations

from google.adk import Agent

from .actions import ACTION_TOOLS
from .budget import enforce_token_budget, record_token_usage
from .guardrails import handle_model_error, handle_tool_error, secure_tool_output, validate_actions
from .memory import KNOWLEDGE_TOOLS
from .model import build_model
from .pii import redact_request_pii, redact_response_pii
from .tools import ALL_TOOLS

# The diagnosis specialist: read-only by construction.
diagnosis_agent = Agent(
    model=build_model(),
    name="diagnosis_agent",
    description="Specialist that diagnoses a specific incident using its runbook and service status.",
    instruction=(
        "You are a diagnosis specialist. Given an incident id, use get_incident for its details and "
        "runbook, get_runbook for the runbook body, and get_service_status for the service. Explain "
        "the likely root cause in a few sentences and cite the runbook. You cannot take actions — "
        "hand your findings back to the coordinator."
    ),
    tools=[*ALL_TOOLS, *KNOWLEDGE_TOOLS],
    before_model_callback=[enforce_token_budget, redact_request_pii],
    after_model_callback=[record_token_usage, redact_response_pii],
    after_tool_callback=secure_tool_output,
    on_model_error_callback=handle_model_error,
    on_tool_error_callback=handle_tool_error,
)

# The remediation specialist: write-only by construction. It executes a diagnosed,
# runbook-backed plan through the guarded actions (each still pauses for human
# approval with a rationale) but holds no log or runbook readers.
remediation_agent = Agent(
    model=build_model(),
    name="remediation_agent",
    description="Specialist that executes approved remediation through the guarded actions.",
    instruction=(
        "You are a remediation specialist. The coordinator hands you a diagnosed incident and a "
        "runbook-backed plan. Propose the exact guarded action (restart_service or resolve_incident), "
        "wait for the engineer's approval, then execute it and report the audit result. Never act "
        "without a diagnosis; never invent targets."
    ),
    tools=[*ACTION_TOOLS],
    before_model_callback=[enforce_token_budget, redact_request_pii],
    after_model_callback=[record_token_usage, redact_response_pii],
    before_tool_callback=validate_actions,
    after_tool_callback=secure_tool_output,
    on_model_error_callback=handle_model_error,
    on_tool_error_callback=handle_tool_error,
)

# The coordinator: triages, then routes — diagnosis first, remediation only after.
coordinator_agent = Agent(
    model=build_model(),
    name="coordinator_agent",
    description="On-call coordinator that triages incidents and delegates diagnosis and remediation.",
    instruction=(
        "You are the on-call coordinator. Triage with list_incidents and get_service_status. When a "
        "specific incident needs a root-cause analysis, delegate to the diagnosis_agent sub-agent. "
        "Once a diagnosis is confirmed and the engineer wants to act, delegate to the "
        "remediation_agent sub-agent with the incident id and the runbook-backed plan, then "
        "summarize the outcome (including the audit record) for the engineer."
    ),
    tools=ALL_TOOLS,
    sub_agents=[diagnosis_agent, remediation_agent],
    before_model_callback=[enforce_token_budget, redact_request_pii],
    after_model_callback=[record_token_usage, redact_response_pii],
    after_tool_callback=secure_tool_output,
    on_model_error_callback=handle_model_error,
    on_tool_error_callback=handle_tool_error,
)
