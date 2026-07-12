"""Offline consistency checks for the shared eval set (Ch. 4.4).

The evalset references dataset entities by id; when the seed data evolves,
these checks catch dangling references before a model-backed eval ever runs.
"""

import json
from pathlib import Path

import pytest

from agent import data
from agent.models import TriageReport

_EVALSET = Path(__file__).parents[1] / "evals" / "ops.evalset.json"
_REPORT_EVALSET = Path(__file__).parents[1] / "evals" / "triage-report.evalset.json"
_CONFIG = Path(__file__).parents[1] / "evals" / "test_config.json"

# Tool-argument keys that reference dataset entities, per tool name.
_INCIDENT_ARGS = {"get_incident": "incident_id", "resolve_incident": "incident_id"}
_SERVICE_ARGS = {"get_service_status": "name", "restart_service": "name", "search_service_logs": "service"}
_RUNBOOK_ARGS = {"get_runbook": "slug"}

# Negative cases deliberately reference entities that must NOT exist.
_EXPECTED_MISSING = {"INC-999", "warehouse"}


def _evalset() -> dict:
    return json.loads(_EVALSET.read_text(encoding="utf-8"))


def _tool_uses():
    for case in _evalset()["eval_cases"]:
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
    for eval_id, use in _tool_uses():
        name, args = use["name"], use["args"]
        if name in _INCIDENT_ARGS and (value := args.get(_INCIDENT_ARGS[name])):
            assert value in incidents or value in _EXPECTED_MISSING, (eval_id, value)
        if name in _SERVICE_ARGS and (value := args.get(_SERVICE_ARGS[name])):
            assert value in services or value in _EXPECTED_MISSING, (eval_id, value)
        if name in _RUNBOOK_ARGS and (value := args.get(_RUNBOOK_ARGS[name])):
            assert value in runbooks, (eval_id, value)


def test_negative_cases_reference_entities_that_stay_missing() -> None:
    """The negative cases lose their point if the dataset ever grows these ids."""
    incidents = {incident.id for incident in data.list_incidents()}
    services = {service.name for service in data.list_services()}
    assert "INC-999" not in incidents
    assert "warehouse" not in services


def test_eval_config_uses_in_order_trajectory_matching() -> None:
    config = json.loads(_CONFIG.read_text(encoding="utf-8"))
    criterion = config["criteria"]["tool_trajectory_avg_score"]
    assert criterion == {"threshold": 1.0, "match_type": "IN_ORDER"}


def test_structured_report_eval_exercises_a_valid_typed_response() -> None:
    evalset = json.loads(_REPORT_EVALSET.read_text(encoding="utf-8"))
    assert len(evalset["eval_cases"]) == 1
    turn = evalset["eval_cases"][0]["conversation"][0]
    text = turn["final_response"]["parts"][0]["text"]
    report = TriageReport.model_validate_json(text)
    assert report.incident_id == "INC-002"
    assert [use["name"] for use in turn["intermediate_data"]["tool_uses"]] == [
        "get_incident",
        "search_service_logs",
        "get_runbook",
    ]


@pytest.mark.parametrize(
    ("actual", "expected", "matches"),
    [
        ([{"name": "a", "args": {}}], [{"name": "a", "args": {}}], True),
        ([{"name": "x", "args": {}}, {"name": "a", "args": {}}], [{"name": "a", "args": {}}], True),
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
