"""Unit tests for the guarded mock actions and the input-validation guardrail (Ch. 4.5)."""

from typing import cast

from google.adk.tools.function_tool import FunctionTool
from google.adk.tools.tool_context import ToolContext

from agent import actions, data, guardrails, tools

# The guardrail only reads tool.name and never touches the context, so a cast None is enough.
_NO_CONTEXT = cast("ToolContext", None)
_ACTIONS_BY_NAME = {tool.name: tool for tool in actions.ACTION_TOOLS}


def test_restart_service_flips_status_and_audits() -> None:
    before = data.get_service("inventory")
    assert before is not None
    assert before["status"] == "down"
    result = actions.restart_service("inventory")
    assert "result" in result
    after = data.get_service("inventory")
    assert after is not None
    assert after["status"] == "operational"
    assert result["audit"]["action"] == "restart_service"


def test_restart_unknown_service_errors() -> None:
    result = actions.restart_service("ghost")
    assert "error" in result


def test_resolve_incident_marks_resolved() -> None:
    before = data.get_incident("INC-002")
    assert before is not None
    assert before["status"] == "open"
    result = actions.resolve_incident("INC-002")
    assert "result" in result
    incident = data.get_incident("INC-002")
    assert incident is not None
    assert incident["status"] == "resolved"
    assert incident["resolved_at"]


def test_resolve_already_resolved_errors() -> None:
    result = actions.resolve_incident("INC-003")  # seeded as resolved
    assert "error" in result


def test_action_tools_require_confirmation() -> None:
    for tool in actions.ACTION_TOOLS:
        assert tool.name in {"restart_service", "resolve_incident"}
        assert tool._require_confirmation is True  # noqa: SLF001 — asserting the HITL guard is on


def test_guardrail_blocks_bad_incident_id() -> None:
    tool = _ACTIONS_BY_NAME["resolve_incident"]
    blocked = guardrails.validate_actions(tool, {"incident_id": "oops"}, _NO_CONTEXT)
    assert blocked is not None
    assert "error" in blocked


def test_guardrail_allows_valid_incident_id() -> None:
    tool = _ACTIONS_BY_NAME["resolve_incident"]
    assert guardrails.validate_actions(tool, {"incident_id": "INC-002"}, _NO_CONTEXT) is None


def test_guardrail_ignores_read_tools() -> None:
    read_tool = FunctionTool(func=tools.list_incidents)
    assert guardrails.validate_actions(read_tool, {}, _NO_CONTEXT) is None
