"""Offline consistency checks for the shared eval sets (Ch. 4.4).

The evalsets reference dataset entities by id; when the seed data evolves,
these checks catch dangling references before a model-backed eval ever runs.
"""

import json
import re
from pathlib import Path

import pytest

from agent import data
from agent.models import TriageReport
from evals.run_adk_eval import REQUIRED_LIVE_CASES
from tests.domain import REFERENCE_DOMAIN

_EVALSET = Path(__file__).parents[1] / "evals" / "ops.evalset.json"
_REPORT_EVALSET = Path(__file__).parents[1] / "evals" / "triage-report.evalset.json"
_WORKFLOW_EVALSET = Path(__file__).parents[1] / "evals" / "workflow.evalset.json"
_EVALSETS = (_EVALSET, _REPORT_EVALSET, _WORKFLOW_EVALSET)
_CONFIG = Path(__file__).parents[1] / "evals" / "test_config.json"
_MISE = Path(__file__).parents[1] / "mise.toml"
_SKILLS = Path(__file__).parents[2] / "data" / "skills"

# Tool-argument keys that reference dataset entities, per tool name.
_INCIDENT_ARGS = {
    "get_incident": "incident_id",
    "recall_incident_context": "incident_id",
    "resolve_incident": "incident_id",
    "save_incident_note": "incident_id",
}
_SERVICE_ARGS = {"get_service_status": "name", "restart_service": "name", "search_service_logs": "service"}
_RUNBOOK_ARGS = {"get_runbook": "slug"}
_SKILL_ARGS = {"load_skill": "skill_name"}

# Negative cases deliberately reference entities that must NOT exist.
_EXPECTED_MISSING = {"INC-999", "warehouse"}


def _evalset() -> dict:
    return json.loads(_EVALSET.read_text(encoding="utf-8"))


def _tool_uses():
    for path in _EVALSETS:
        document = json.loads(path.read_text(encoding="utf-8"))
        for case in document["eval_cases"]:
            for turn in case["conversation"]:
                for use in turn["intermediate_data"]["tool_uses"]:
                    yield case["eval_id"], use


def test_evalset_has_a_representative_size() -> None:
    assert len(_evalset()["eval_cases"]) >= 12


def test_eval_ids_are_unique() -> None:
    ids = [case["eval_id"] for case in _evalset()["eval_cases"]]
    assert len(ids) == len(set(ids))


def test_every_case_has_turns_with_text_and_expected_response() -> None:
    for case in _evalset()["eval_cases"]:
        assert case["conversation"], case["eval_id"]
        for turn in case["conversation"]:
            assert turn["user_content"]["parts"][0]["text"].strip()
            assert turn["final_response"]["parts"][0]["text"].strip()


def test_referenced_entities_exist_in_the_seed_data() -> None:
    """Every referenced incident/service/runbook exists — unless it is a negative case."""
    incidents = {incident.id for incident in data.list_incidents()}
    services = {service.name for service in data.list_services()}
    runbooks = set(data.list_runbook_slugs())
    skills = {path.parent.name for path in _SKILLS.glob("*/SKILL.md")}
    for eval_id, use in _tool_uses():
        name, args = use["name"], use["args"]
        if name in _INCIDENT_ARGS and (value := args.get(_INCIDENT_ARGS[name])):
            assert value in incidents or value in _EXPECTED_MISSING, (eval_id, value)
        if name in _SERVICE_ARGS and (value := args.get(_SERVICE_ARGS[name])):
            assert value in services or value in _EXPECTED_MISSING, (eval_id, value)
        if name in _RUNBOOK_ARGS and (value := args.get(_RUNBOOK_ARGS[name])):
            assert value in runbooks, (eval_id, value)
        if name in _SKILL_ARGS and (value := args.get(_SKILL_ARGS[name])):
            assert value in skills, (eval_id, value)


def test_negative_cases_reference_entities_that_stay_missing() -> None:
    """The negative cases lose their point if the dataset ever grows these ids."""
    incidents = {incident.id for incident in data.list_incidents()}
    services = {service.name for service in data.list_services()}
    assert "INC-999" not in incidents
    assert "warehouse" not in services


def test_eval_config_uses_in_order_trajectory_matching() -> None:
    config = json.loads(_CONFIG.read_text(encoding="utf-8"))
    criterion = config["criteria"]["tool_trajectory_avg_score"]
    assert criterion["match_type"] == "IN_ORDER"
    # Each case is strict. run_adk_eval.py applies the separately documented
    # aggregate case-pass floor over ADK's final pass/fail tally.
    assert criterion["threshold"] == 1.0
    custom = config["custom_metrics"]["tool_trajectory_avg_score"]
    assert custom["code_config"]["name"] == "evals.required_trajectory.evaluate_required_tool_trajectory"


def test_every_load_bearing_behavior_is_a_required_live_case() -> None:
    task = _MISE.read_text(encoding="utf-8")
    assert "--min-pass-rate 0.33" in task
    assert tuple(re.findall(r"--required-case ([a-z0-9-]+)", task)) == REQUIRED_LIVE_CASES


def test_behavioral_cases_require_the_evidence_and_memory_tools_they_claim() -> None:
    cases = {case["eval_id"]: case for case in _evalset()["eval_cases"]}

    def tool_names(eval_id: str, turn: int = 0) -> list[str]:
        uses = cases[eval_id]["conversation"][turn]["intermediate_data"]["tool_uses"]
        return [use["name"] for use in uses]

    assert tool_names("diagnose-with-logs") == ["get_incident", "search_service_logs", "get_runbook"]
    assert tool_names("recommend-fix") == ["get_incident", "search_service_logs", "get_runbook"]
    assert tool_names("investigation-recalls-context") == ["recall_incident_context", "get_incident"]
    assert tool_names("remediation-loads-skill") == ["list_skills", "load_skill"]
    assert tool_names("ambiguous-symptom") == ["search_runbooks"]
    assert tool_names("memory-note-recall", 0) == ["save_incident_note"]
    assert tool_names("memory-note-recall", 1) == ["recall_incident_context"]
    assert tool_names("restart-needs-approval") == [
        "get_incident",
        "get_service_status",
        "search_service_logs",
        "get_runbook",
        "restart_service",
    ]
    restart_case = cases["restart-needs-approval"]["conversation"][0]
    assert restart_case["intermediate_data"]["tool_uses"][2]["args"] == {
        "query": "",
        "service": "inventory",
    }
    assert tool_names("resolve-needs-approval") == [
        "get_incident",
        "get_service_status",
        "get_runbook",
        "resolve_incident",
    ]
    injection_tools = tool_names("injection-restart-rejected")
    assert injection_tools == ["search_service_logs"]
    assert not {"restart_service", "resolve_incident"} & set(injection_tools)

    memory_prompt = cases["investigation-recalls-context"]["conversation"][0]["user_content"]["parts"][0]["text"]
    assert memory_prompt == f"Investigate {REFERENCE_DOMAIN.incidents.inventory_down}."
    assert "recall_incident_context" not in memory_prompt
    assert "get_incident" not in memory_prompt

    prompts = {
        eval_id: cases[eval_id]["conversation"][0]["user_content"]["parts"][0]["text"]
        for eval_id in ("restart-needs-approval", "resolve-needs-approval")
    }
    assert "search_service_logs with only the service and no query" in prompts["restart-needs-approval"]
    assert "guarded restart_service tool" in prompts["restart-needs-approval"]
    assert "guarded resolve_incident tool" in prompts["resolve-needs-approval"]
    assert all("built-in confirmation request" in prompt for prompt in prompts.values())
    assert all("Work sequentially" in prompt for prompt in prompts.values())
    assert all("do not guess them or batch dependent calls" in prompt for prompt in prompts.values())
    assert all("Wait for" in prompt and "read results" in prompt for prompt in prompts.values())
    assert "unless you emit the restart_service call" in prompts["restart-needs-approval"]
    assert "unless you emit the resolve_incident call" in prompts["resolve-needs-approval"]


def test_structured_report_eval_exercises_a_valid_typed_response() -> None:
    evalset = json.loads(_REPORT_EVALSET.read_text(encoding="utf-8"))
    assert len(evalset["eval_cases"]) == 1
    turn = evalset["eval_cases"][0]["conversation"][0]
    text = turn["final_response"]["parts"][0]["text"]
    report = TriageReport.model_validate_json(text)
    assert report.incident_id == "INC-002"
    incident = data.get_incident(report.incident_id)
    assert incident is not None
    tool_uses = turn["intermediate_data"]["tool_uses"]
    assert [use["name"] for use in tool_uses] == [
        "get_incident",
        "search_service_logs",
        "get_runbook",
    ]
    assert tool_uses[1]["args"]["service"] == incident.service
    assert tool_uses[2]["args"]["slug"] == incident.runbook


def test_workflow_eval_exercises_plan_review_and_read_only_evidence() -> None:
    evalset = json.loads(_WORKFLOW_EVALSET.read_text(encoding="utf-8"))
    assert len(evalset["eval_cases"]) == 1
    case = evalset["eval_cases"][0]
    assert case["session_input"]["app_name"] == "triage_workflow"
    turn = case["conversation"][0]
    assert "INC-001" in turn["user_content"]["parts"][0]["text"]
    incident = data.get_incident("INC-001")
    assert incident is not None
    tool_uses = turn["intermediate_data"]["tool_uses"]
    assert [use["name"] for use in tool_uses] == [
        "get_incident",
        "get_service_status",
        "search_service_logs",
        "get_runbook",
    ]
    assert tool_uses[1]["args"]["name"] == incident.service
    assert tool_uses[2]["args"]["service"] == incident.service
    assert tool_uses[3]["args"]["slug"] == incident.runbook
    assert not {"restart_service", "resolve_incident", "save_incident_note"} & {use["name"] for use in tool_uses}


@pytest.mark.parametrize(
    ("actual", "expected", "matches"),
    [
        ([{"name": "a", "args": {}}], [{"name": "a", "args": {}}], True),
        ([{"name": "x", "args": {}}, {"name": "a", "args": {}}], [{"name": "a", "args": {}}], True),
        (
            [{"name": "search", "args": {"service": "inventory", "query": "error", "limit": 10}}],
            [{"name": "search", "args": {"service": "inventory"}}],
            True,
        ),
        (
            [{"name": "search", "args": {"service": "checkout", "query": "error"}}],
            [{"name": "search", "args": {"service": "inventory"}}],
            False,
        ),
        (
            [{"name": "b", "args": {}}, {"name": "a", "args": {}}],
            [{"name": "a", "args": {}}, {"name": "b", "args": {}}],
            False,
        ),
        ([], [{"name": "a", "args": {}}], False),
        ([{"name": "a", "args": {}}], [], True),
    ],
)
def test_mlflow_scorer_in_order_semantics(actual, expected, matches) -> None:
    from evals.mlflow_eval import _in_order

    assert _in_order(actual, expected) is matches
