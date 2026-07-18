"""Offline tests for the token/cost regression comparison logic."""

from evals import cost_eval


def test_no_regression_within_tolerance() -> None:
    baseline = {"lookup": {"total_tokens": 1000, "model_calls": 3}}
    observed = {"lookup": {"total_tokens": 1200, "model_calls": 3}}  # +20%, under the 25% default
    assert cost_eval.regressions(observed, baseline) == []


def test_flags_token_growth_beyond_tolerance() -> None:
    baseline = {"lookup": {"total_tokens": 1000, "model_calls": 3}}
    observed = {"lookup": {"total_tokens": 1400, "model_calls": 3}}  # +40%
    problems = cost_eval.regressions(observed, baseline)
    assert len(problems) == 1
    assert "lookup total_tokens" in problems[0]


def test_flags_extra_model_call() -> None:
    baseline = {"triage": {"total_tokens": 500, "model_calls": 2}}
    observed = {"triage": {"total_tokens": 500, "model_calls": 4}}  # doubled calls
    problems = cost_eval.regressions(observed, baseline)
    assert any("model_calls" in line for line in problems)


def test_missing_case_is_not_a_regression() -> None:
    # A renamed or removed case simply has no observation; it must not fail the gate.
    baseline = {"gone": {"total_tokens": 900, "model_calls": 2}}
    assert cost_eval.regressions({}, baseline) == []


def test_zero_baseline_never_regresses() -> None:
    # A baseline of 0 has no meaningful ratio, so any observation is allowed.
    baseline = {"new": {"total_tokens": 0, "model_calls": 0}}
    observed = {"new": {"total_tokens": 5000, "model_calls": 9}}
    assert cost_eval.regressions(observed, baseline) == []


def test_tolerance_is_configurable() -> None:
    baseline = {"lookup": {"total_tokens": 1000, "model_calls": 3}}
    observed = {"lookup": {"total_tokens": 1050, "model_calls": 3}}  # +5%
    assert cost_eval.regressions(observed, baseline, tolerance=0.0)  # zero tolerance flags it
    assert cost_eval.regressions(observed, baseline, tolerance=0.10) == []  # 10% absorbs it
