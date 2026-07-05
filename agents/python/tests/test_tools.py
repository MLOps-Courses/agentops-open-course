"""Unit tests for the Ops Copilot function tools (Ch. 3.1), run against the bundled dataset."""

from agent import actions, data, memory, tools
from agent.agent import root_agent


def test_list_incidents_returns_all_by_default() -> None:
    result = tools.list_incidents()
    assert result["count"] == len(result["incidents"])
    assert result["count"] >= 6  # seeded incidents
    assert {"id", "service", "title", "severity", "status"} <= result["incidents"][0].keys()


def test_list_incidents_filters_by_status() -> None:
    result = tools.list_incidents(status="open")
    assert result["count"] >= 1
    assert all(incident["status"] == "open" for incident in result["incidents"])


def test_list_incidents_filters_by_service() -> None:
    result = tools.list_incidents(service="checkout")
    assert result["count"] >= 1
    assert all(incident["service"] == "checkout" for incident in result["incidents"])


def test_get_incident_known_and_unknown() -> None:
    known = tools.get_incident("INC-001")
    assert known["incident"]["id"] == "INC-001"
    assert known["incident"]["runbook"] == "high-latency"

    unknown = tools.get_incident("INC-999")
    assert "error" in unknown


def test_get_service_status_includes_open_incidents() -> None:
    result = tools.get_service_status("checkout")
    assert result["service"]["status"] == "degraded"
    assert all(incident["status"] != "resolved" for incident in result["open_incidents"])


def test_get_service_status_unknown_lists_known() -> None:
    result = tools.get_service_status("nope")
    assert "error" in result
    assert "checkout" in result["error"]


def test_append_audit_writes_entry() -> None:
    before = len(data.list_incidents())  # touch the DB to ensure it is readable
    entry = data.append_audit(actor="test", action="noop", target="checkout", detail="unit test")
    assert entry["actor"] == "test"
    assert entry["ts"].endswith("Z")
    assert before >= 6


def test_agent_registers_tools() -> None:
    assert root_agent.name == "agentops_agent"
    expected = len(tools.ALL_TOOLS) + len(memory.KNOWLEDGE_TOOLS) + len(actions.ACTION_TOOLS)
    assert len(root_agent.tools) == expected
