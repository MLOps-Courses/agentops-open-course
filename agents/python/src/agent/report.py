"""Structured outputs — a typed ``TriageReport`` with explicit fallbacks (Chapter 4.0).

Free prose is fine for conversation, but downstream automation needs a schema.
``triage_report_agent`` uses ADK's ``output_schema`` so its final answer is
validated JSON (tools stay available during the thought loop). The programmatic
path handles violations explicitly: retry once with the validation errors fed
back, then degrade to prose with a logged, counted telemetry event — never a
silent crash and never a silently-wrong object.
"""

from __future__ import annotations

import logging
from collections.abc import Awaitable, Callable
from json import dumps

from google.adk import Agent
from opentelemetry import metrics
from pydantic import ValidationError

from .budget import enforce_token_budget, record_token_usage
from .guardrails import handle_model_error, handle_tool_error, secure_tool_output
from .memory import KNOWLEDGE_TOOLS
from .model import build_model
from .models import TriageReport
from .pii import redact_request_pii, redact_response_pii
from .tools import ALL_TOOLS

logger = logging.getLogger(__name__)

# Counted so dashboards can watch schema-violation rates (a model-quality signal).
_SCHEMA_FAILURES = metrics.get_meter("agentops.agent").create_counter(
    "agentops.triage_report.schema_failures",
    unit="1",
    description="Triage reports that failed schema validation after one retry",
)

REPORT_INSTRUCTION = """\
You produce a machine-consumable triage report for one incident.
Use get_incident for the record, search_service_logs for evidence, and get_runbook
for the remediation guidance. Fill every field of the TriageReport schema from tool
output only — never invent ids, services, or log lines. Respond with the JSON object
only: no prose, no Markdown fences.
"""

# The structured entry point. The conversational root_agent stays unchanged;
# this agent's final answer must validate against TriageReport.
triage_report_agent = Agent(
    model=build_model(),
    name="triage_report_agent",
    description="Produces a schema-validated triage report for a single incident.",
    instruction=REPORT_INSTRUCTION,
    tools=[*ALL_TOOLS, *KNOWLEDGE_TOOLS],
    output_schema=TriageReport,
    before_model_callback=[enforce_token_budget, redact_request_pii],
    after_model_callback=[record_token_usage, redact_response_pii],
    after_tool_callback=secure_tool_output,
    on_model_error_callback=handle_model_error,
    on_tool_error_callback=handle_tool_error,
)


def parse_triage_report(text: str) -> TriageReport:
    """Parse model text into a validated ``TriageReport``.

    Tolerates the one formatting quirk local models add anyway — a Markdown
    code fence — and lets every real violation raise ``ValidationError``.
    """
    cleaned = text.strip()
    if cleaned.startswith("```"):
        first_newline = cleaned.index("\n") if "\n" in cleaned else len(cleaned)
        cleaned = cleaned[first_newline:].strip().removesuffix("```").strip()
    return TriageReport.model_validate_json(cleaned)


def report_prompt(incident_id: str) -> str:
    """Build the structured-output request for one incident."""
    schema = dumps(TriageReport.model_json_schema(), separators=(",", ":"))
    return (
        f"Produce the triage report for incident {incident_id}. "
        f"Respond with a single JSON object matching this schema exactly:\n{schema}"
    )


async def request_triage_report(
    generate: Callable[[str], Awaitable[str]],
    incident_id: str,
) -> TriageReport | str:
    """Request a validated report; retry once on violation, then degrade to prose.

    ``generate`` is the model boundary (a prompt-in/text-out coroutine), so the
    fallback policy is deterministic to test offline with a fake model.
    """
    prompt = report_prompt(incident_id)
    first = await generate(prompt)
    try:
        return parse_triage_report(first)
    except ValidationError as error:
        retry_prompt = (
            f"{prompt}\n\nYour previous reply failed schema validation:\n{error}\n"
            "Fix these errors and respond with only the corrected JSON object."
        )
    second = await generate(retry_prompt)
    try:
        return parse_triage_report(second)
    except ValidationError:
        _SCHEMA_FAILURES.add(1)
        logger.warning("Triage report for %s failed schema validation twice; degrading to prose", incident_id)
        return second
