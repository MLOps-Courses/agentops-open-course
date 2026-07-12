"""Unit tests for the guarded mock actions and the input-validation guardrail (Ch. 4.5)."""

import asyncio
import sqlite3
from contextlib import closing
from types import SimpleNamespace
from typing import cast

import pytest
from google.adk.agents.callback_context import CallbackContext
from google.adk.models.llm_request import LlmRequest
from google.adk.tools.function_tool import FunctionTool
from google.adk.tools.tool_context import ToolContext

from agent import actions, data, guardrails, tools

# The guardrail only reads tool.name and never touches the context, so a cast None is enough.
_NO_CONTEXT = cast("ToolContext", None)
_NO_CALLBACK_CONTEXT = cast("CallbackContext", None)
_NO_LLM_REQUEST = cast("LlmRequest", None)
_ACTIONS_BY_NAME = {tool.name: tool for tool in actions.ACTION_TOOLS}


def test_restart_service_flips_status_and_audits() -> None:
    before = data.get_service("inventory")
    assert before is not None
    assert before.status == "down"
    result = actions.restart_service("inventory")
    assert "result" in result
    after = data.get_service("inventory")
    assert after is not None
    assert after.status == "operational"
    assert result["audit"]["action"] == "restart_service"
    assert result["audit"]["approved_by"] == "direct-call"


def test_restart_unknown_service_errors() -> None:
    result = actions.restart_service("ghost")
    assert "error" in result


def test_restart_rejects_malformed_service() -> None:
    assert "error" in actions.restart_service("../../inventory")


def test_restart_handles_concurrent_service_removal(monkeypatch) -> None:
    monkeypatch.setattr(data, "restart_service_with_audit", lambda *_args, **_kwargs: None)
    assert "error" in actions.restart_service("inventory")


def test_resolve_incident_marks_resolved() -> None:
    before = data.get_incident("INC-002")
    assert before is not None
    assert before.status == "open"
    result = actions.resolve_incident("INC-002")
    assert "result" in result
    incident = data.get_incident("INC-002")
    assert incident is not None
    assert incident.status == "resolved"
    assert incident.resolved_at


def test_resolve_already_resolved_errors() -> None:
    result = actions.resolve_incident("INC-003")  # seeded as resolved
    assert "error" in result


def test_resolve_unknown_incident_errors() -> None:
    result = actions.resolve_incident("INC-999")  # not in the seeded dataset
    assert "error" in result
    assert "INC-999" in result["error"]


def test_resolve_rejects_malformed_incident_id() -> None:
    assert "error" in actions.resolve_incident("INC-../../passwd")


def test_resolve_handles_concurrent_resolution(monkeypatch) -> None:
    monkeypatch.setattr(data, "resolve_incident_with_audit", lambda *_args, **_kwargs: None)
    assert "error" in actions.resolve_incident("INC-002")


def _approved_context(payload: object) -> ToolContext:
    """A fake ToolContext carrying an ADK confirmation with the given payload."""
    return cast(
        "ToolContext",
        SimpleNamespace(
            user_id="engineer",
            session=SimpleNamespace(id="session-7"),
            invocation_id="invocation-9",
            tool_confirmation=SimpleNamespace(confirmed=True, payload=payload),
        ),
    )


def test_action_audit_records_approver_rationale_and_context() -> None:
    context = _approved_context({"rationale": "inventory is hard down; approved in the incident call"})
    result = actions.restart_service("inventory", context)
    audit = result["audit"]
    assert audit["approved_by"] == "engineer"
    assert audit["session_id"] == "session-7"
    assert audit["invocation_id"] == "invocation-9"
    assert audit["rationale"] == "inventory is hard down; approved in the incident call"
    assert "inventory was down" in audit["context_summary"]
    assert "INC-002" in audit["context_summary"]  # the open incident the approver saw


def test_action_accepts_a_bare_string_rationale() -> None:
    result = actions.resolve_incident("INC-002", _approved_context("fixed by the 09:40 rollback"))
    assert result["audit"]["rationale"] == "fixed by the 09:40 rollback"
    assert "SEV1" in result["audit"]["context_summary"]


def test_action_rejects_a_missing_rationale() -> None:
    for payload in (None, {}, {"rationale": "   "}, ""):
        result = actions.restart_service("inventory", _approved_context(payload))
        assert "error" in result, payload
        assert "rationale" in result["error"]
    service = data.get_service("inventory")
    assert service is not None
    assert service.status == "down"  # the refused action changed nothing


def test_direct_calls_are_attributed_as_direct() -> None:
    result = actions.restart_service("inventory")
    assert result["audit"]["approved_by"] == "direct-call"
    assert result["audit"]["rationale"] == "direct-call"


def test_action_tools_require_confirmation() -> None:
    for tool in actions.ACTION_TOOLS:
        assert tool.name in {"restart_service", "resolve_incident"}
        assert tool._require_confirmation is True  # noqa: SLF001 — asserting the HITL guard is on


def test_unconfirmed_call_pauses_for_approval() -> None:
    """First invocation: ADK asks the human instead of running the function."""
    requested: list[str] = []
    context = cast(
        "ToolContext",
        SimpleNamespace(
            tool_confirmation=None,
            actions=SimpleNamespace(skip_summarization=False),
            request_confirmation=lambda hint: requested.append(hint),
        ),
    )
    tool = _ACTIONS_BY_NAME["restart_service"]
    result = asyncio.run(tool.run_async(args={"name": "inventory"}, tool_context=context))
    assert "requires confirmation" in result["error"]
    assert requested  # the approval request went out
    service = data.get_service("inventory")
    assert service is not None
    assert service.status == "down"  # nothing ran


def test_rejected_confirmation_blocks_the_action() -> None:
    context = cast(
        "ToolContext",
        SimpleNamespace(tool_confirmation=SimpleNamespace(confirmed=False, payload=None)),
    )
    tool = _ACTIONS_BY_NAME["restart_service"]
    result = asyncio.run(tool.run_async(args={"name": "inventory"}, tool_context=context))
    assert result == {"error": "This tool call is rejected."}
    service = data.get_service("inventory")
    assert service is not None
    assert service.status == "down"


def test_guardrail_blocks_bad_incident_id() -> None:
    tool = _ACTIONS_BY_NAME["resolve_incident"]
    blocked = guardrails.validate_actions(tool, {"incident_id": "oops"}, _NO_CONTEXT)
    assert blocked is not None
    assert "error" in blocked


def test_guardrail_allows_valid_incident_id() -> None:
    tool = _ACTIONS_BY_NAME["resolve_incident"]
    args = {"incident_id": " inc-002 "}
    assert guardrails.validate_actions(tool, args, _NO_CONTEXT) is None
    assert args["incident_id"] == "INC-002"


def test_guardrail_blocks_empty_service_name() -> None:
    tool = _ACTIONS_BY_NAME["restart_service"]
    blocked = guardrails.validate_actions(tool, {"name": "   "}, _NO_CONTEXT)
    assert blocked is not None
    assert "error" in blocked


def test_guardrail_allows_valid_service_name() -> None:
    tool = _ACTIONS_BY_NAME["restart_service"]
    args = {"name": " Inventory "}
    assert guardrails.validate_actions(tool, args, _NO_CONTEXT) is None
    assert args["name"] == "inventory"


def test_guardrail_ignores_read_tools() -> None:
    read_tool = FunctionTool(func=tools.list_incidents)
    assert guardrails.validate_actions(read_tool, {}, _NO_CONTEXT) is None


def test_audit_log_is_append_only() -> None:
    actions.restart_service("inventory")
    with (
        closing(sqlite3.connect(data.db_path())) as connection,
        pytest.raises(sqlite3.IntegrityError, match="append-only"),
    ):
        connection.execute("UPDATE audit_log SET actor = 'attacker'")


def test_action_and_audit_roll_back_together() -> None:
    with closing(sqlite3.connect(data.db_path())) as connection:
        connection.execute(
            "CREATE TRIGGER reject_audit BEFORE INSERT ON audit_log BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END"
        )
        connection.commit()
    with pytest.raises(data.DataAccessError, match="SQLite operation failed"):
        actions.restart_service("inventory")
    service = data.get_service("inventory")
    assert service is not None
    assert service.status == "down"


def test_error_callbacks_return_safe_responses(caplog) -> None:
    tool = _ACTIONS_BY_NAME["restart_service"]
    try:
        raise RuntimeError("secret detail")
    except RuntimeError as error:
        tool_result = guardrails.handle_tool_error(tool, {}, _NO_CONTEXT, error)
    assert "failed safely" in tool_result["error"]
    assert "secret detail" not in tool_result["error"]

    try:
        raise RuntimeError("provider detail")
    except RuntimeError as error:
        model_result = guardrails.handle_model_error(_NO_CALLBACK_CONTEXT, _NO_LLM_REQUEST, error)
    assert model_result.error_code == "MODEL_UNAVAILABLE"
    assert model_result.content is not None
    assert model_result.content.parts is not None
    assert "provider is unavailable" in (model_result.content.parts[0].text or "")
    assert "RuntimeError: secret detail" in caplog.text
    assert "RuntimeError: provider detail" in caplog.text
    assert "NoneType: None" not in caplog.text
