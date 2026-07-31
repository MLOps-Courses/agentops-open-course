"""Offline tests for required-argument trajectory matching."""

import json
from pathlib import Path

import pytest
from google.adk.evaluation.eval_case import IntermediateData, Invocation
from google.adk.evaluation.eval_metrics import EvalMetric, ToolTrajectoryCriterion
from google.adk.evaluation.evaluator import EvalStatus
from google.genai import types

from agent.composition import INSTRUCTION
from evals.required_trajectory import (
    RequiredToolTrajectoryEvaluator,
    evaluate_required_tool_trajectory,
    required_tools_in_order,
)
from tests.domain import REFERENCE_DOMAIN

_CHECKOUT_INCIDENT = REFERENCE_DOMAIN.incidents.checkout_latency
_INVENTORY = REFERENCE_DOMAIN.services.inventory
_INVENTORY_INCIDENT = REFERENCE_DOMAIN.incidents.inventory_down
_PAYMENTS = REFERENCE_DOMAIN.services.payments
_SERVICE_DOWN_RUNBOOK = REFERENCE_DOMAIN.runbooks.service_down


def _call(tool_name: str, **args) -> types.FunctionCall:
    return types.FunctionCall(name=tool_name, args=args)


def _invocation(*calls: types.FunctionCall) -> Invocation:
    return Invocation(
        user_content=types.Content(role="user", parts=[types.Part(text="test")]),
        intermediate_data=IntermediateData(tool_uses=list(calls)),
    )


def _metric(match_type: str = "IN_ORDER") -> EvalMetric:
    return EvalMetric(
        metric_name="tool_trajectory_avg_score",
        criterion=ToolTrajectoryCriterion(threshold=1.0, match_type=match_type),
    )


def test_required_calls_allow_extra_calls_and_optional_arguments() -> None:
    actual = [
        _call("list_incidents", status="open"),
        _call("get_incident", incident_id=_INVENTORY_INCIDENT),
        _call("search_service_logs", service=_INVENTORY, query="crash-loop", limit=10),
        _call("get_service_status", name=_INVENTORY),
        _call("get_runbook", slug=_SERVICE_DOWN_RUNBOOK),
    ]
    expected = [
        _call("get_incident", incident_id=_INVENTORY_INCIDENT),
        _call("search_service_logs", service=_INVENTORY),
        _call("get_runbook", slug=_SERVICE_DOWN_RUNBOOK),
    ]
    assert required_tools_in_order(actual, expected)


def test_recommend_fix_instruction_and_eval_require_the_linked_runbook() -> None:
    """Deleting the paired instruction rule must turn the Chapter 2 drill red."""
    rule_lines = INSTRUCTION.splitlines()
    rule_start = next(
        index for index, line in enumerate(rule_lines) if line.startswith("- To recommend a fix, consult the runbooks:")
    )
    paired_rule = "\n".join(rule_lines[rule_start : rule_start + 2])
    evalset = json.loads((Path(__file__).parents[1] / "evals" / "ops.evalset.json").read_text(encoding="utf-8"))
    recommend_fix = next(case for case in evalset["eval_cases"] if case["eval_id"] == "recommend-fix")
    tool_names = [
        call["name"]
        for invocation in recommend_fix["conversation"]
        for call in invocation["intermediate_data"]["tool_uses"]
    ]

    assert "`get_runbook`" in paired_rule
    assert "get_runbook" in tool_names


def test_required_empty_string_accepts_default_omission_but_rejects_a_filter() -> None:
    expected = [_call("search_service_logs", service=_INVENTORY, query="")]
    assert required_tools_in_order([_call("search_service_logs", service=_INVENTORY)], expected)
    assert required_tools_in_order([_call("search_service_logs", service=_INVENTORY, query="")], expected)
    assert not required_tools_in_order(
        [_call("search_service_logs", service=_INVENTORY, query="crash")],
        expected,
    )


def test_required_calls_reject_wrong_values_and_order() -> None:
    expected = [
        _call("get_incident", incident_id=_INVENTORY_INCIDENT),
        _call("get_runbook", slug=_SERVICE_DOWN_RUNBOOK),
    ]
    assert not required_tools_in_order([_call("get_incident", incident_id=_CHECKOUT_INCIDENT)], expected)
    assert not required_tools_in_order(list(reversed(expected)), expected)


def test_required_calls_compare_nested_required_values() -> None:
    actual = [_call("query", filters={"service": _INVENTORY, "status": "open"}, limit=10)]
    expected = [_call("query", filters={"service": _INVENTORY})]
    assert required_tools_in_order(actual, expected)


def test_required_values_do_not_confuse_json_booleans_and_numbers() -> None:
    assert not required_tools_in_order([_call("query", limit=True)], [_call("query", limit=1)])
    assert not required_tools_in_order([_call("query", limit=1)], [_call("query", limit=True)])
    assert not required_tools_in_order([_call("query", flags=[True])], [_call("query", flags=[1])])


def test_required_lists_compare_nested_values_recursively() -> None:
    actual = [_call("query", filters=[{"service": _INVENTORY, "status": "open"}])]
    expected = [_call("query", filters=[{"service": _INVENTORY}])]
    assert required_tools_in_order(actual, expected)
    assert not required_tools_in_order(actual, [_call("query", filters=[{"service": _PAYMENTS}])])
    assert not required_tools_in_order(actual, [_call("query", filters=[])])


def test_adk_adapter_scores_each_invocation_strictly() -> None:
    expected = [_invocation(_call("get_incident", incident_id=_INVENTORY_INCIDENT))]
    passing = [_invocation(_call("get_incident", incident_id=_INVENTORY_INCIDENT, detail=True))]
    result = evaluate_required_tool_trajectory(_metric(), passing, expected)
    assert result.overall_score == 1.0
    assert result.overall_eval_status is EvalStatus.PASSED
    assert result.per_invocation_results[0].eval_status is EvalStatus.PASSED

    failing = [_invocation(_call("get_incident", incident_id=_CHECKOUT_INCIDENT))]
    result = evaluate_required_tool_trajectory(_metric(), failing, expected)
    assert result.overall_score == 0.0
    assert result.overall_eval_status is EvalStatus.FAILED


def test_adk_adapter_handles_empty_or_mismatched_invocations() -> None:
    evaluator = RequiredToolTrajectoryEvaluator(_metric())
    assert evaluator.evaluate_invocations([], []).overall_eval_status is EvalStatus.NOT_EVALUATED
    with pytest.raises(ValueError, match="invocation counts"):
        evaluator.evaluate_invocations([_invocation()], [_invocation(), _invocation()])
    with pytest.raises(ValueError, match="expected_invocations"):
        evaluator.evaluate_invocations([_invocation()])


def test_adk_adapter_rejects_unsupported_match_type() -> None:
    with pytest.raises(ValueError, match="only IN_ORDER"):
        RequiredToolTrajectoryEvaluator(_metric("EXACT"))
