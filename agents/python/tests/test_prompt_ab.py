"""Offline tests for the prompt A/B comparison formatting."""

from evals import prompt_ab
from evals.prompt_ab import format_comparison


def test_format_comparison_reports_every_scorer_and_delta() -> None:
    scores_a = {"tool_trajectory": 1.0, "complete_conversation": 1.0, "response_facts": 0.8, "tool_policy": 1.0}
    scores_b = {"tool_trajectory": 1.0, "complete_conversation": 1.0, "response_facts": 1.0, "tool_policy": 1.0}
    table = format_comparison("v1", scores_a, "v2", scores_b)
    lines = table.splitlines()
    assert lines[0].split() == ["scorer", "v1", "v2", "delta"]
    assert len(lines) == 1 + len(prompt_ab.DETERMINISTIC_SCORERS)
    response_facts_row = next(line for line in lines if line.startswith("response_facts"))
    assert "+0.20" in response_facts_row  # v2 improved facts coverage


def test_format_comparison_shows_regression_as_negative_delta() -> None:
    scores_a = dict.fromkeys(prompt_ab.DETERMINISTIC_SCORERS, 1.0)
    scores_b = {**scores_a, "tool_policy": 0.5}
    table = format_comparison("current", scores_a, "candidate", scores_b)
    policy_row = next(line for line in table.splitlines() if line.startswith("tool_policy"))
    assert "-0.50" in policy_row
