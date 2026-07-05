"""Unit tests for the triage → diagnose → recommend workflow (Ch. 3.5)."""

from agent.workflow import diagnose, recommend, triage, triage_workflow


def test_workflow_chains_three_steps_in_order() -> None:
    assert triage_workflow.name == "triage_workflow"
    assert [triage.name, diagnose.name, recommend.name] == ["triage", "diagnose", "recommend"]
    source, *steps = triage_workflow.edges[0]
    assert source == "START"
    assert steps == [triage, diagnose, recommend]


def test_each_step_has_a_model_and_tools() -> None:
    for step in (triage, diagnose, recommend):
        assert step.model
        assert step.tools  # every step is grounded in tools
