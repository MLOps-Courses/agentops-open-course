"""Smoke test: the reference agent is importable and defined."""

from agent.agent import root_agent


def test_root_agent_defined() -> None:
    assert root_agent.name == "agentops_agent"
    assert root_agent.model
