"""Unit tests for the AgentOps Agent function tools (Ch. 3.1), run against the bundled dataset."""

from typing import cast

from google.adk.agents.llm_agent import ToolUnion

from agent import actions, data, memory, tools
from agent import agent as agent_module
from agent.agent import root_agent
from agent.longterm import MEMORY_TOOLS


def test_list_incidents_returns_all_by_default() -> None:
    result = tools.list_incidents()
    assert result["count"] == len(result["incidents"])
    assert result["count"] >= 6  # seeded incidents
    assert {"id", "service", "title", "severity", "status", "opened_at", "summary"} <= result["incidents"][0].keys()


def test_list_incidents_filters_by_status() -> None:
    result = tools.list_incidents(status="open")
    assert result["count"] >= 1
    assert all(incident["status"] == "open" for incident in result["incidents"])


def test_list_incidents_rejects_invalid_status() -> None:
    result = tools.list_incidents(status="pending")
    assert "error" in result


def test_list_incidents_filters_by_service() -> None:
    result = tools.list_incidents(service="checkout")
    assert result["count"] >= 1
    assert all(incident["service"] == "checkout" for incident in result["incidents"])


def test_list_incidents_rejects_invalid_service() -> None:
    assert "error" in tools.list_incidents(service="../checkout")


def test_get_incident_known_and_unknown() -> None:
    known = tools.get_incident("INC-001")
    assert known["incident"]["id"] == "INC-001"
    assert known["incident"]["runbook"] == "high-latency"

    unknown = tools.get_incident("INC-999")
    assert "error" in unknown
    assert "error" in tools.get_incident("../INC-001")


def test_get_service_status_includes_open_incidents() -> None:
    result = tools.get_service_status("checkout")
    assert result["service"]["status"] == "degraded"
    assert all(incident["status"] != "resolved" for incident in result["open_incidents"])


def test_get_service_status_unknown_lists_known() -> None:
    result = tools.get_service_status("nope")
    assert "error" in result
    assert "checkout" in result["error"]
    assert "error" in tools.get_service_status("../checkout")


def test_search_service_logs_filters_and_orders() -> None:
    result = tools.search_service_logs("inventory", query="ERROR", limit=2)
    assert result["count"] == 2
    assert all("ERROR" in line for line in result["lines"])
    assert "stock lookup" in result["lines"][0]


def test_search_service_logs_rejects_bad_inputs() -> None:
    assert "error" in tools.search_service_logs("../../etc")
    assert "error" in tools.search_service_logs("checkout", limit=0)
    assert "error" in tools.search_service_logs("payments")


def test_append_audit_writes_entry() -> None:
    before = len(data.list_incidents())  # touch the DB to ensure it is readable
    entry = data.append_audit(
        actor="test",
        approved_by="engineer",
        rationale="unit-test approval",
        context_summary="unit-test context",
        session_id="session-1",
        invocation_id="invocation-1",
        action="noop",
        target="checkout",
        detail="unit test",
    )
    assert entry.actor == "test"
    assert entry.approved_by == "engineer"
    assert entry.rationale == "unit-test approval"
    assert entry.ts.endswith("Z")
    assert before >= 6


def test_agent_registers_tools() -> None:
    assert root_agent.name == "agentops_agent"
    # read + knowledge + guarded actions + long-term memory tools + the skill toolset
    expected = len(tools.ALL_TOOLS) + len(memory.KNOWLEDGE_TOOLS) + len(actions.ACTION_TOOLS) + len(MEMORY_TOOLS) + 1
    assert len(root_agent.tools) == expected


def test_agent_uses_governed_mcp_read_tools_when_configured(monkeypatch) -> None:
    sentinel = cast("ToolUnion", object())
    monkeypatch.setattr(agent_module.settings, "mcp_url", "http://agentgateway:3000/mcp")
    monkeypatch.setattr(agent_module, "ops_mcp_toolset", lambda _url: sentinel)
    assert agent_module._read_tools() == [sentinel]  # noqa: SLF001 - composition contract
