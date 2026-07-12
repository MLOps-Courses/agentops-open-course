"""Unit tests for the structured TriageReport path and its fallback policy."""

import asyncio
import json
import logging

import pytest
from pydantic import ValidationError

from agent import report
from agent.models import TriageReport
from agent.structured_report.agent import root_agent as report_eval_agent

_VALID = {
    "incident_id": "INC-002",
    "severity": "SEV1",
    "affected_services": ["inventory"],
    "hypothesis": "Pods crash-loop after the bad deploy exhausted memory.",
    "evidence": ["inventory pods restart every 30s", "HTTP 503 on stock lookups"],
    "recommended_runbook": "service-down",
    "proposed_actions": ["restart_service inventory (needs approval)"],
}


def test_valid_report_parses() -> None:
    parsed = report.parse_triage_report(json.dumps(_VALID))
    assert isinstance(parsed, TriageReport)
    assert parsed.incident_id == "INC-002"


def test_markdown_fenced_report_parses() -> None:
    fenced = f"```json\n{json.dumps(_VALID)}\n```"
    assert report.parse_triage_report(fenced).recommended_runbook == "service-down"


def test_schema_rejects_extra_fields_and_bad_ids() -> None:
    with pytest.raises(ValidationError):
        report.parse_triage_report(json.dumps({**_VALID, "surprise": True}))
    with pytest.raises(ValidationError):
        report.parse_triage_report(json.dumps({**_VALID, "incident_id": "ticket-2"}))


def test_first_valid_answer_needs_no_retry() -> None:
    calls: list[str] = []

    async def generate(prompt: str) -> str:
        calls.append(prompt)
        return json.dumps(_VALID)

    result = asyncio.run(report.request_triage_report(generate, "INC-002"))
    assert isinstance(result, TriageReport)
    assert len(calls) == 1
    assert "JSON object matching this schema" in calls[0]


def test_violation_retries_with_the_errors_fed_back() -> None:
    calls: list[str] = []

    async def generate(prompt: str) -> str:
        calls.append(prompt)
        if len(calls) == 1:
            return json.dumps({**_VALID, "incident_id": "not-an-id"})
        return json.dumps(_VALID)

    result = asyncio.run(report.request_triage_report(generate, "INC-002"))
    assert isinstance(result, TriageReport)
    assert len(calls) == 2
    assert "failed schema validation" in calls[1]
    assert "incident_id" in calls[1]  # the validation error names the field


def test_double_violation_degrades_to_prose_and_counts(monkeypatch, caplog) -> None:
    class _Counter:
        def __init__(self) -> None:
            self.total = 0

        def add(self, amount: int) -> None:
            self.total += amount

    counter = _Counter()
    monkeypatch.setattr(report, "_SCHEMA_FAILURES", counter)

    async def generate(prompt: str) -> str:
        del prompt
        return "The incident looks bad, please restart inventory."

    with caplog.at_level(logging.WARNING):
        result = asyncio.run(report.request_triage_report(generate, "INC-002"))
    assert result == "The incident looks bad, please restart inventory."
    assert counter.total == 1
    assert any("degrading to prose" in message for message in caplog.messages)


def test_report_agent_enforces_the_schema() -> None:
    assert report.triage_report_agent.output_schema is TriageReport
    assert report_eval_agent is report.triage_report_agent
    tool_names = {getattr(tool, "__name__", "") for tool in report.triage_report_agent.tools}
    assert "restart_service" not in tool_names  # structured path stays read-only
