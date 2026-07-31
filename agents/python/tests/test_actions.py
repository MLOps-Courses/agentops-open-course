"""Unit tests for the guarded mock actions and the input-validation guardrail (Ch. 4.5)."""

import asyncio
import sqlite3
from contextlib import closing
from types import SimpleNamespace
from typing import Any, cast

import pytest
from google.adk.agents.callback_context import CallbackContext
from google.adk.models.llm_request import LlmRequest
from google.adk.tools.function_tool import FunctionTool
from google.adk.tools.tool_context import ToolContext

from agent import actions, data, guardrails, tools
from agent.circuit import CircuitOpenError
from agent.models import CURRENT_AUDIT_SCHEMA_VERSION, MAX_AUDIT_RATIONALE_LENGTH
from agent.resilience import ToolDeadlineError
from tests.domain import REFERENCE_DOMAIN

# The guardrail only reads tool.name and never touches the context, so a cast None is enough.
_NO_CONTEXT = cast("ToolContext", None)
_NO_CALLBACK_CONTEXT = cast("CallbackContext", None)
_NO_LLM_REQUEST = cast("LlmRequest", None)
_ACTIONS_BY_NAME = {tool.name: tool for tool in actions.ACTION_TOOLS}
_INVENTORY = REFERENCE_DOMAIN.services.inventory
_INVENTORY_INCIDENT = REFERENCE_DOMAIN.incidents.inventory_down
_RESOLVED_INCIDENT = REFERENCE_DOMAIN.incidents.payments_errors


def _approved_context(
    payload: object,
    *,
    confirmed: bool = True,
    user_id: str | None = "engineer",
    session_id: str | None = "session-7",
    invocation_id: str | None = "invocation-9",
) -> ToolContext:
    """A fake ToolContext carrying an ADK confirmation with the given payload."""
    return cast(
        "ToolContext",
        SimpleNamespace(
            user_id=user_id,
            session=SimpleNamespace(id=session_id) if session_id is not None else None,
            invocation_id=invocation_id,
            tool_confirmation=SimpleNamespace(confirmed=confirmed, payload=payload),
        ),
    )


def _run_action(name: str, args: dict[str, Any], context: ToolContext) -> dict[str, Any]:
    return cast("dict[str, Any]", asyncio.run(_ACTIONS_BY_NAME[name].run_async(args=args, tool_context=context)))


def _audit_count() -> int:
    with closing(sqlite3.connect(data.db_path())) as connection:
        row = connection.execute("SELECT COUNT(*) FROM audit_log").fetchone()
    assert row is not None
    return int(row[0])


def test_restart_service_flips_status_and_audits() -> None:
    before = data.get_service(_INVENTORY)
    assert before is not None
    assert before.status == "down"
    result = _run_action(
        "restart_service",
        {"name": _INVENTORY},
        _approved_context({"rationale": f"{_INVENTORY} is down during the incident"}),
    )
    assert "result" in result
    after = data.get_service(_INVENTORY)
    assert after is not None
    assert after.status == "operational"
    assert result["audit"]["action"] == "restart_service"
    assert result["audit"]["approved_by"] == "engineer"
    assert result["audit"]["schema_version"] == CURRENT_AUDIT_SCHEMA_VERSION


def test_replayed_approved_restart_returns_original_audit_and_changes_state_once() -> None:
    context = _approved_context({"rationale": f"{_INVENTORY} is down during the incident"})
    first = _run_action("restart_service", {"name": _INVENTORY}, context)
    assert "result" in first

    # Prove a replay does not run UPDATE again by moving the service to a newer
    # state between deliveries of the same approved invocation.
    with closing(sqlite3.connect(data.db_path())) as connection:
        connection.execute("UPDATE services SET status = 'degraded' WHERE name = ?", (_INVENTORY,))
        connection.commit()

    replay = _run_action("restart_service", {"name": _INVENTORY}, context)
    assert replay["audit"] == first["audit"]
    service = data.get_service(_INVENTORY)
    assert service is not None
    assert service.status == "degraded"
    assert _audit_count() == 1


def test_restart_unknown_service_errors() -> None:
    result = actions.restart_service("ghost")
    assert "error" in result


def test_restart_rejects_malformed_service() -> None:
    assert "error" in actions.restart_service("../../inventory")


def test_restart_handles_concurrent_service_removal(monkeypatch) -> None:
    monkeypatch.setattr(data, "restart_service_with_audit", lambda *_args, **_kwargs: None)
    result = _run_action(
        "restart_service",
        {"name": _INVENTORY},
        _approved_context({"rationale": f"{_INVENTORY} is still down"}),
    )
    assert "error" in result


def test_resolve_incident_marks_resolved() -> None:
    before = data.get_incident(_INVENTORY_INCIDENT)
    assert before is not None
    assert before.status == "open"
    result = _run_action(
        "resolve_incident",
        {"incident_id": _INVENTORY_INCIDENT},
        _approved_context({"rationale": "the rollback restored service"}),
    )
    assert "result" in result
    incident = data.get_incident(_INVENTORY_INCIDENT)
    assert incident is not None
    assert incident.status == "resolved"
    assert incident.resolved_at


def test_resolve_already_resolved_errors() -> None:
    result = _run_action(
        "resolve_incident",
        {"incident_id": _RESOLVED_INCIDENT},
        _approved_context({"rationale": "verify the resolved incident"}),
    )
    assert "error" in result


def test_resolve_unknown_incident_errors() -> None:
    result = actions.resolve_incident("INC-999")  # not in the seeded dataset
    assert "error" in result
    assert "INC-999" in result["error"]


def test_resolve_rejects_malformed_incident_id() -> None:
    assert "error" in actions.resolve_incident("INC-../../passwd")


def test_resolve_handles_concurrent_resolution(monkeypatch) -> None:
    monkeypatch.setattr(data, "resolve_incident_with_audit", lambda *_args, **_kwargs: None)
    result = _run_action(
        "resolve_incident",
        {"incident_id": _INVENTORY_INCIDENT},
        _approved_context({"rationale": "the incident appears recovered"}),
    )
    assert "error" in result


def test_action_audit_records_approver_rationale_and_context() -> None:
    rationale = f"{_INVENTORY} is hard down; approved in the incident call"
    context = _approved_context({"rationale": rationale})
    result = _run_action("restart_service", {"name": _INVENTORY}, context)
    audit = result["audit"]
    assert audit["approved_by"] == "engineer"
    assert audit["session_id"] == "session-7"
    assert audit["invocation_id"] == "invocation-9"
    assert audit["rationale"] == rationale
    assert f"{_INVENTORY} was down" in audit["context_summary"]
    assert _INVENTORY_INCIDENT in audit["context_summary"]  # current open incident captured with the action


def test_action_accepts_a_bare_string_rationale() -> None:
    result = _run_action(
        "resolve_incident",
        {"incident_id": _INVENTORY_INCIDENT},
        _approved_context("fixed by the 09:40 rollback"),
    )
    assert result["audit"]["rationale"] == "fixed by the 09:40 rollback"
    assert "SEV1" in result["audit"]["context_summary"]


def test_action_redacts_pii_and_credentials_before_audit_persistence() -> None:
    rationale = f"{_INVENTORY_INCIDENT} approved by jane.doe@acme.com with token=super-secret-token-123456"
    result = _run_action(
        "restart_service",
        {"name": _INVENTORY},
        _approved_context({"rationale": rationale}),
    )
    persisted = result["audit"]["rationale"]
    assert _INVENTORY_INCIDENT in persisted
    assert "jane.doe@acme.com" not in persisted
    assert "super-secret-token-123456" not in persisted
    assert "<EMAIL_ADDRESS>" in persisted
    assert "token=<SECRET>" in persisted


def test_action_rejects_an_overlong_rationale_without_mutation_or_audit() -> None:
    audit_count = _audit_count()
    result = _run_action(
        "restart_service",
        {"name": _INVENTORY},
        _approved_context({"rationale": "x" * (MAX_AUDIT_RATIONALE_LENGTH + 1)}),
    )
    assert "error" in result
    assert str(MAX_AUDIT_RATIONALE_LENGTH) in result["error"]
    service = data.get_service(_INVENTORY)
    assert service is not None
    assert service.status == "down"
    assert _audit_count() == audit_count


def test_action_rejects_a_missing_rationale() -> None:
    for payload in (None, {}, {"rationale": "   "}, ""):
        result = _run_action("restart_service", {"name": _INVENTORY}, _approved_context(payload))
        assert "error" in result, payload
        assert "rationale" in result["error"]
    service = data.get_service(_INVENTORY)
    assert service is not None
    assert service.status == "down"  # the refused action changed nothing


def test_direct_calls_fail_closed_without_mutating() -> None:
    result = actions.restart_service(_INVENTORY)
    assert "error" in result
    assert "confirmation flow" in result["error"]
    service = data.get_service(_INVENTORY)
    assert service is not None
    assert service.status == "down"


@pytest.mark.parametrize(
    ("context", "missing"),
    [
        (_approved_context({"rationale": "approved"}, user_id=None), "approver identity"),
        (_approved_context({"rationale": "approved"}, session_id=None), "session id"),
        (_approved_context({"rationale": "approved"}, invocation_id=None), "invocation id"),
    ],
)
def test_confirmed_actions_require_attributable_identity(context, missing) -> None:
    result = _run_action("restart_service", {"name": _INVENTORY}, context)
    assert "error" in result
    assert missing in result["error"]
    service = data.get_service(_INVENTORY)
    assert service is not None
    assert service.status == "down"


def test_public_function_rechecks_confirmation_when_called_directly() -> None:
    result = actions.restart_service(
        _INVENTORY,
        _approved_context({"rationale": "not actually approved"}, confirmed=False),
    )
    assert "error" in result
    assert "has not been confirmed" in result["error"]
    service = data.get_service(_INVENTORY)
    assert service is not None
    assert service.status == "down"


def test_action_tools_require_confirmation() -> None:
    for tool in actions.ACTION_TOOLS:
        assert tool.name in {"restart_service", "resolve_incident"}
        assert tool._require_confirmation is True  # noqa: SLF001 — asserting the HITL guard is on
        declaration = tool._get_declaration()  # noqa: SLF001 — inspect model-facing schema
        assert declaration is not None
        description = declaration.description or ""
        assert "Create an ADK confirmation request" in description
        assert "Call this tool before approval" in description
        assert "call does not" in description
        assert "A prose promise cannot create the confirmation request" in description


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
    result = asyncio.run(tool.run_async(args={"name": _INVENTORY}, tool_context=context))
    assert "requires confirmation" in result["error"]
    assert requested  # the approval request went out
    service = data.get_service(_INVENTORY)
    assert service is not None
    assert service.status == "down"  # nothing ran


def test_rejected_confirmation_blocks_the_action() -> None:
    context = cast(
        "ToolContext",
        SimpleNamespace(tool_confirmation=SimpleNamespace(confirmed=False, payload=None)),
    )
    tool = _ACTIONS_BY_NAME["restart_service"]
    result = asyncio.run(tool.run_async(args={"name": _INVENTORY}, tool_context=context))
    assert result == {"error": "This tool call is rejected."}
    service = data.get_service(_INVENTORY)
    assert service is not None
    assert service.status == "down"


def test_guardrail_blocks_bad_incident_id() -> None:
    tool = _ACTIONS_BY_NAME["resolve_incident"]
    blocked = guardrails.validate_actions(tool, {"incident_id": "oops"}, _NO_CONTEXT)
    assert blocked is not None
    assert "error" in blocked


def test_guardrail_allows_valid_incident_id() -> None:
    tool = _ACTIONS_BY_NAME["resolve_incident"]
    args = {"incident_id": f" {_INVENTORY_INCIDENT.lower()} "}
    assert guardrails.validate_actions(tool, args, _NO_CONTEXT) is None
    assert args["incident_id"] == _INVENTORY_INCIDENT


def test_guardrail_blocks_empty_service_name() -> None:
    tool = _ACTIONS_BY_NAME["restart_service"]
    blocked = guardrails.validate_actions(tool, {"name": "   "}, _NO_CONTEXT)
    assert blocked is not None
    assert "error" in blocked


def test_guardrail_allows_valid_service_name() -> None:
    tool = _ACTIONS_BY_NAME["restart_service"]
    args = {"name": f" {_INVENTORY.title()} "}
    assert guardrails.validate_actions(tool, args, _NO_CONTEXT) is None
    assert args["name"] == _INVENTORY


def test_guardrail_ignores_read_tools() -> None:
    read_tool = FunctionTool(func=tools.list_incidents)
    assert guardrails.validate_actions(read_tool, {}, _NO_CONTEXT) is None


def test_audit_log_is_append_only() -> None:
    _run_action(
        "restart_service",
        {"name": _INVENTORY},
        _approved_context({"rationale": f"{_INVENTORY} is hard down"}),
    )
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
        data.restart_service_with_audit(
            _INVENTORY,
            actor="agentops-agent",
            approved_by="engineer",
            rationale=f"{_INVENTORY} is hard down",
            session_id="session-7",
            invocation_id="invocation-9",
        )
    service = data.get_service(_INVENTORY)
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


def test_kill_switch_freezes_writes_before_approval(monkeypatch) -> None:
    """AGENT_WRITES_DISABLED refuses every guarded write with no state change."""
    monkeypatch.setattr(actions.settings, "writes_disabled", True)
    before = _audit_count()
    restart = _run_action("restart_service", {"name": _INVENTORY}, _approved_context({"rationale": "runbook says so"}))
    resolve = _run_action(
        "resolve_incident",
        {"incident_id": _INVENTORY_INCIDENT},
        _approved_context({"rationale": "fixed"}),
    )
    assert "AGENT_WRITES_DISABLED" in restart["error"]
    assert "AGENT_WRITES_DISABLED" in resolve["error"]
    assert _audit_count() == before  # frozen: nothing was written or audited


def test_kill_switch_leaves_reads_and_approvals_working_when_clear(monkeypatch) -> None:
    """With the switch clear, an approved write still succeeds normally."""
    monkeypatch.setattr(actions.settings, "writes_disabled", False)
    result = _run_action(
        "restart_service",
        {"name": _INVENTORY},
        _approved_context({"rationale": "runbook says so"}),
    )
    assert "restarted" in result["result"]


def test_first_party_tool_errors_reach_the_caller_verbatim() -> None:
    """Classify what is safe to surface; do not answer every failure with silence.

    ``resilience.py`` and ``circuit.py`` author messages that name the exact knob to turn.
    Collapsing them into "inspect the service logs" left the on-call engineer with less
    information than the process already had.
    """
    tool = _ACTIONS_BY_NAME["restart_service"]

    for error in (
        CircuitOpenError("Tool 'get_incident' circuit is open after repeated failures; retrying in at most 30s."),
        ToolDeadlineError("Tool 'get_incident' exceeded its 30s deadline (AGENT_TOOL_TIMEOUT_S)."),
        data.DataAccessError("Cannot open the incident database at /srv/state/runtime.db"),
    ):
        result = guardrails.handle_tool_error(tool, {}, _NO_CONTEXT, error)
        assert result["error"] == str(error)
        assert "failed safely" not in result["error"]

    # An arbitrary exception stays opaque: its message may carry a query, path, or driver detail.
    opaque = guardrails.handle_tool_error(tool, {}, _NO_CONTEXT, RuntimeError("SELECT * FROM secrets"))
    assert "failed safely" in opaque["error"]
    assert "SELECT" not in opaque["error"]
