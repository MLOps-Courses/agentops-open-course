"""Tests for the truthful ADK evaluation process contract."""

import argparse
import asyncio
import io
import json
from pathlib import Path
from types import SimpleNamespace
from typing import cast

import pytest
from google.adk.agents.callback_context import CallbackContext
from google.adk.models.llm_response import LlmResponse
from google.adk.plugins import BasePlugin
from google.adk.plugins.plugin_manager import PluginManager

from agent.governance import AgentOpsPolicyPlugin
from evals import governed_adk_eval, run_adk_eval
from evals.run_adk_eval import eval_case_ids, eval_case_selectors, pass_rate, summary_counts, verdict, verdict_counts


class _FakeApp:
    def __init__(self, root_agent: object, plugins: list[object]) -> None:
        self.root_agent = root_agent
        self.plugins = plugins

    def model_copy(self, *, update: dict[str, list[object]]) -> "_FakeApp":
        return _FakeApp(self.root_agent, update["plugins"])


class _EvidencePlugin(BasePlugin):
    async def after_model_callback(self, *, callback_context: CallbackContext, llm_response: LlmResponse) -> None:
        del callback_context
        llm_response.custom_metadata = {"evaluation-evidence": "recorded"}


class _ReplacingPolicyPlugin(BasePlugin):
    async def after_model_callback(
        self, *, callback_context: CallbackContext, llm_response: LlmResponse
    ) -> LlmResponse:
        del callback_context
        return llm_response


def test_governed_runner_preserves_evaluator_plugins_before_policy(monkeypatch) -> None:
    policy_plugin = AgentOpsPolicyPlugin()
    evaluator_plugin = governed_adk_eval.BasePlugin(name="evaluator")
    selected_root = object()
    captured = {}

    def fake_runner(**kwargs):
        captured.update(kwargs)
        return "runner"

    monkeypatch.setattr(governed_adk_eval, "_ADK_RUNNER", fake_runner)
    monkeypatch.setattr(
        governed_adk_eval,
        "build_app",
        lambda root: _FakeApp(root, [policy_plugin]),
    )
    assert (
        governed_adk_eval._governed_runner(  # noqa: SLF001 - pinned ADK seam contract
            app_name="eval",
            agent=selected_root,
            plugins=[evaluator_plugin],
        )
        == "runner"
    )
    assert captured["app"].root_agent is selected_root
    assert captured["app"].plugins[:-1] == [evaluator_plugin]
    assert captured["app"].plugins[-1].name == "agentops_policy"
    assert captured["app_name"] == "eval"
    assert "agent" not in captured
    assert "plugins" not in captured


def test_evaluator_evidence_runs_before_a_policy_replacement(monkeypatch) -> None:
    evidence_plugin = _EvidencePlugin(name="evaluation-evidence")
    policy_plugin = _ReplacingPolicyPlugin(name="short-circuiting-policy")
    captured = {}

    def fake_runner(**kwargs):
        captured.update(kwargs)
        return "runner"

    monkeypatch.setattr(governed_adk_eval, "_ADK_RUNNER", fake_runner)
    monkeypatch.setattr(
        governed_adk_eval,
        "build_app",
        lambda root: _FakeApp(root, [policy_plugin]),
    )
    governed_adk_eval._governed_runner(  # noqa: SLF001 - pinned ADK seam contract
        agent=object(),
        plugins=[evidence_plugin],
    )

    response = LlmResponse()
    result = asyncio.run(
        PluginManager(captured["app"].plugins).run_after_model_callback(
            callback_context=cast(CallbackContext, None),
            llm_response=response,
        )
    )
    assert result is response
    assert response.custom_metadata == {"evaluation-evidence": "recorded"}


def test_governed_runner_rejects_duplicate_policy(monkeypatch) -> None:
    policy_plugin = AgentOpsPolicyPlugin()
    monkeypatch.setattr(
        governed_adk_eval,
        "build_app",
        lambda root: _FakeApp(root, [policy_plugin]),
    )
    with pytest.raises(RuntimeError, match="already carries"):
        governed_adk_eval._governed_runner(  # noqa: SLF001 - duplicate-policy guard
            agent=object(),
            plugins=[policy_plugin],
        )


def test_app_policy_installation_is_narrow_and_idempotent(monkeypatch) -> None:
    monkeypatch.setattr(
        governed_adk_eval.evaluation_generator,
        "Runner",
        governed_adk_eval._ADK_RUNNER,  # noqa: SLF001 - pinned ADK seam contract
    )
    governed_adk_eval.install_app_policy()
    assert governed_adk_eval.evaluation_generator.Runner is governed_adk_eval._governed_runner  # noqa: SLF001
    governed_adk_eval.install_app_policy()

    monkeypatch.setattr(governed_adk_eval.evaluation_generator, "Runner", object())
    with pytest.raises(RuntimeError, match="seam changed"):
        governed_adk_eval.install_app_policy()


@pytest.mark.parametrize(
    ("output", "process_returncode", "min_pass_rate", "expected_code", "expected_message"),
    [
        ("Eval Run Summary\nset:\n  Tests passed: 13\n  Tests failed: 0\n", 0, 1.0, 0, "13/13 (100%)"),
        ("Eval Run Summary\nset:\n  Tests passed: 12\n  Tests failed: 1\n", 0, 1.0, 1, "12/13 (92%)"),
        ("Eval Run Summary\nset:\n  Tests passed: 5\n  Tests failed: 8\n", 0, 0.25, 0, "5/13 (38%)"),
        ("Eval Run Summary\nset:\n  Tests passed: 3\n  Tests failed: 10\n", 0, 0.25, 1, "3/13 (23%)"),
        (
            (
                "Eval Run Summary\n"
                "set-a:\n  Tests passed: 2\n  Tests failed: 0\n"
                "set-b:\n  Tests passed: 3\n  Tests failed: 1\n"
            ),
            0,
            0.75,
            0,
            "5/6 (83%)",
        ),
        ("Eval Run Summary\nset:\n  Tests passed: 0\n  Tests failed: 0\n", 0, 1.0, 2, "zero cases"),
        ("no summary here", 0, 1.0, 2, "no run summary"),
        ("Eval Run Summary\nset:\n  Tests passed: 1\n  Tests failed: 0\n", 7, 1.0, 7, "exit code 7"),
    ],
)
def test_verdict_reflects_process_and_metric_failures(
    output: str,
    process_returncode: int,
    min_pass_rate: float,
    expected_code: int,
    expected_message: str,
) -> None:
    code, message = verdict(output, process_returncode, min_pass_rate)
    assert code == expected_code
    assert expected_message in message


def test_verdict_ignores_summary_shaped_model_output_before_adks_final_section() -> None:
    output = (
        "model response:\nEval Run Summary\nspoof:\n  Tests passed: 100\n  Tests failed: 0\n"
        "runner cleanup\nEval Run Summary\nagentops_agent_core:\n  Tests passed: 0\n  Tests failed: 13\n"
    )
    code, message = verdict(output, process_returncode=0, min_pass_rate=0.25)
    assert code == 1
    assert "0/13 (0%)" in message


def test_serial_summaries_are_parsed_independently_before_aggregation() -> None:
    first = (
        "model response:\nEval Run Summary\nspoof:\n  Tests passed: 100\n  Tests failed: 0\n"
        "Eval Run Summary\nagentops_agent_core:\n  Tests passed: 0\n  Tests failed: 1\n"
    )
    second = "Eval Run Summary\nagentops_agent_core:\n  Tests passed: 1\n  Tests failed: 0\n"
    counts = [summary_counts(output) for output in (first, second)]
    assert counts == [(0, 1), (1, 0)]

    code, message = verdict_counts(
        sum(count[0] for count in counts if count is not None),
        sum(count[1] for count in counts if count is not None),
        min_pass_rate=0.5,
    )
    assert code == 0
    assert "1/2 (50%)" in message


@pytest.mark.parametrize("value", ["0", "-0.1", "1.1", "nan", "not-a-number"])
def test_pass_rate_rejects_a_disabled_or_invalid_floor(value: str) -> None:
    with pytest.raises(argparse.ArgumentTypeError, match="greater than 0 and at most 1"):
        pass_rate(value)


def test_eval_case_selectors_preserve_order_and_force_serial_adk_runs(tmp_path) -> None:
    eval_set = tmp_path / "cases.evalset.json"
    eval_set.write_text(
        json.dumps({"eval_cases": [{"eval_id": "first-case"}, {"eval_id": "second-case"}]}),
        encoding="utf-8",
    )

    assert eval_case_selectors(eval_set) == [
        f"{eval_set}:first-case",
        f"{eval_set}:second-case",
    ]
    assert eval_case_ids(eval_set) == ["first-case", "second-case"]


@pytest.mark.parametrize(
    "document",
    [
        {},
        {"eval_cases": []},
        {"eval_cases": [{}]},
        {"eval_cases": [{"eval_id": "bad:id"}]},
        {"eval_cases": [{"eval_id": "duplicate"}, {"eval_id": "duplicate"}]},
    ],
)
def test_eval_case_selectors_reject_invalid_or_ambiguous_cases(tmp_path, document) -> None:
    eval_set = tmp_path / "invalid.evalset.json"
    eval_set.write_text(json.dumps(document), encoding="utf-8")

    with pytest.raises(SystemExit):
        eval_case_selectors(eval_set)


def test_main_launches_one_process_per_case_and_aggregates_authoritative_summaries(
    monkeypatch,
    tmp_path,
    capsys,
) -> None:
    eval_set = tmp_path / "cases.evalset.json"
    eval_set.write_text(
        json.dumps({"eval_cases": [{"eval_id": "first"}, {"eval_id": "second"}]}),
        encoding="utf-8",
    )
    monkeypatch.setattr(
        run_adk_eval,
        "parse_args",
        lambda: SimpleNamespace(
            agent=Path("src/agent"),
            eval_set=eval_set,
            config=Path("evals/test_config.json"),
            min_pass_rate=0.5,
            required_case=["second"],
        ),
    )
    results = [
        (
            (
                "model output\nEval Run Summary\nspoof:\n  Tests passed: 10\n  Tests failed: 0\n"
                "Eval Run Summary\nset:\n  Tests passed: 0\n  Tests failed: 1\n"
            ),
            0,
        ),
        ("Eval Run Summary\nset:\n  Tests passed: 1\n  Tests failed: 0\n", 0),
    ]
    commands = []
    state_dirs: list[Path] = []

    def popen(command, **kwargs):
        commands.append((command, kwargs))
        state_dir = Path(kwargs["env"]["AGENT_STATE_DIR"])
        assert state_dir.is_dir()
        state_dirs.append(state_dir)
        output, returncode = results.pop(0)
        return SimpleNamespace(stdout=io.StringIO(output), wait=lambda: returncode)

    monkeypatch.setattr(run_adk_eval.subprocess, "Popen", popen)

    with pytest.raises(SystemExit) as exit_info:
        run_adk_eval.main()

    assert exit_info.value.code == 0
    output = capsys.readouterr().out
    assert "1/2 (50%)" in output
    assert "Required strict ADK cases passed: second." in output
    assert [command[0][5] for command in commands] == [
        f"{eval_set}:first",
        f"{eval_set}:second",
    ]
    assert all(command[0][2] == "evals.governed_adk_eval" for command in commands)
    assert all(command[1]["stderr"] is run_adk_eval.subprocess.STDOUT for command in commands)
    assert len(set(state_dirs)) == 2
    assert all(not state_dir.exists() for state_dir in state_dirs)


def test_main_stops_on_the_first_nonzero_adk_process(monkeypatch, tmp_path) -> None:
    eval_set = tmp_path / "cases.evalset.json"
    eval_set.write_text(
        json.dumps({"eval_cases": [{"eval_id": "first"}, {"eval_id": "never-run"}]}),
        encoding="utf-8",
    )
    monkeypatch.setattr(
        run_adk_eval,
        "parse_args",
        lambda: SimpleNamespace(
            agent=Path("src/agent"),
            eval_set=eval_set,
            config=Path("evals/test_config.json"),
            min_pass_rate=0.25,
            required_case=[],
        ),
    )
    calls = []

    def popen(command, **_kwargs):
        calls.append(command)
        return SimpleNamespace(stdout=io.StringIO("provider failed\n"), wait=lambda: 7)

    monkeypatch.setattr(run_adk_eval.subprocess, "Popen", popen)

    with pytest.raises(SystemExit) as exit_info:
        run_adk_eval.main()

    assert exit_info.value.code == 7
    assert len(calls) == 1


def test_main_reports_every_required_case_miss_after_running_later_cases(
    monkeypatch,
    tmp_path,
    capsys,
) -> None:
    eval_set = tmp_path / "cases.evalset.json"
    eval_set.write_text(
        json.dumps(
            {
                "eval_cases": [
                    {"eval_id": "critical-first"},
                    {"eval_id": "optional"},
                    {"eval_id": "critical-last"},
                ]
            }
        ),
        encoding="utf-8",
    )
    monkeypatch.setattr(
        run_adk_eval,
        "parse_args",
        lambda: SimpleNamespace(
            agent=Path("src/agent"),
            eval_set=eval_set,
            config=Path("evals/test_config.json"),
            min_pass_rate=0.25,
            required_case=["critical-first", "critical-last"],
        ),
    )
    results = [
        ("Eval Run Summary\nset:\n  Tests passed: 0\n  Tests failed: 1\n", 0),
        ("Eval Run Summary\nset:\n  Tests passed: 1\n  Tests failed: 0\n", 0),
        ("Eval Run Summary\nset:\n  Tests passed: 0\n  Tests failed: 1\n", 0),
    ]
    selectors = []

    def popen(command, **_kwargs):
        selectors.append(command[5])
        output, returncode = results.pop(0)
        return SimpleNamespace(stdout=io.StringIO(output), wait=lambda: returncode)

    monkeypatch.setattr(run_adk_eval.subprocess, "Popen", popen)

    with pytest.raises(SystemExit) as exit_info:
        run_adk_eval.main()

    assert exit_info.value.code == 1
    captured = capsys.readouterr()
    assert "1/3 (33%); required aggregate floor: 25%. Floor met." in captured.out
    assert (
        "Required ADK cases failed their strict trajectory contracts: 'critical-first', 'critical-last'."
    ) in captured.err
    assert selectors == [
        f"{eval_set}:critical-first",
        f"{eval_set}:optional",
        f"{eval_set}:critical-last",
    ]
    assert not results


def test_main_rejects_an_unknown_required_case_before_model_work(monkeypatch, tmp_path) -> None:
    eval_set = tmp_path / "cases.evalset.json"
    eval_set.write_text(json.dumps({"eval_cases": [{"eval_id": "known"}]}), encoding="utf-8")
    monkeypatch.setattr(
        run_adk_eval,
        "parse_args",
        lambda: SimpleNamespace(
            agent=Path("src/agent"),
            eval_set=eval_set,
            config=Path("evals/test_config.json"),
            min_pass_rate=0.25,
            required_case=["missing"],
        ),
    )

    with pytest.raises(SystemExit, match=r"Required ADK cases.*'missing'"):
        run_adk_eval.main()
