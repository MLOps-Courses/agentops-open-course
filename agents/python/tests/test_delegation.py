"""Unit tests for multi-agent delegation and per-agent least privilege (Ch. 3.6)."""

from agent.delegation import coordinator_agent, diagnosis_agent, remediation_agent

_WRITE_TOOLS = {"restart_service", "resolve_incident"}
_READ_TOOLS = {"list_incidents", "get_incident", "get_service_status", "search_service_logs"}
_KNOWLEDGE = {"get_runbook", "search_runbooks"}


def _tool_names(agent) -> set[str]:
    return {getattr(tool, "name", None) or getattr(tool, "__name__", "") for tool in agent.tools}


def test_coordinator_delegates_to_both_specialists() -> None:
    assert coordinator_agent.name == "coordinator_agent"
    assert coordinator_agent.sub_agents == [diagnosis_agent, remediation_agent]


def test_diagnosis_specialist_is_grounded() -> None:
    assert diagnosis_agent.name == "diagnosis_agent"
    assert _tool_names(diagnosis_agent) >= _READ_TOOLS


def test_delegation_respects_tool_boundaries() -> None:
    """Least privilege by construction: each specialist physically lacks the other's tools."""
    diagnosis_tools = _tool_names(diagnosis_agent)
    remediation_tools = _tool_names(remediation_agent)
    # The diagnosis agent cannot invoke write actions — it does not hold them.
    assert diagnosis_tools & _WRITE_TOOLS == set()
    # The remediation agent cannot read raw logs or runbooks — it does not hold them.
    assert remediation_tools & (_READ_TOOLS | _KNOWLEDGE) == set()
    assert remediation_tools == _WRITE_TOOLS
    # The coordinator itself holds no write tools either: acting requires delegation.
    assert _tool_names(coordinator_agent) & _WRITE_TOOLS == set()


def test_remediation_actions_still_require_confirmation() -> None:
    """Least privilege does not replace HITL: the guarded actions stay guarded."""
    for tool in remediation_agent.tools:
        assert getattr(tool, "_require_confirmation", None) is True  # the HITL contract
