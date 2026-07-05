"""Unit tests for multi-agent delegation (Ch. 3.6)."""

from agent.delegation import coordinator_agent, diagnosis_agent


def test_coordinator_delegates_to_diagnosis() -> None:
    assert coordinator_agent.name == "coordinator_agent"
    assert coordinator_agent.sub_agents == [diagnosis_agent]


def test_diagnosis_specialist_is_grounded() -> None:
    assert diagnosis_agent.name == "diagnosis_agent"
    assert diagnosis_agent.tools  # the specialist has the tools it needs to diagnose
