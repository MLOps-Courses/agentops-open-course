"""Offline tests for the full-conversation MLflow evaluation harness."""

import asyncio
import json
from types import SimpleNamespace
from typing import Any, cast

import pytest
from google.genai import types
from mlflow.entities import Feedback

from evals import mlflow_eval


def test_load_cases_preserves_every_turn_part_and_tool_boundary(tmp_path) -> None:
    path = tmp_path / "multi-turn.evalset.json"
    path.write_text(
        json.dumps(
            {
                "eval_cases": [
                    {
                        "eval_id": "multi",
                        "conversation": [
                            {
                                "user_content": {"parts": [{"text": "First "}, {"text": "turn"}]},
                                "final_response": {"parts": [{"text": "First answer"}]},
                                "intermediate_data": {"tool_uses": [{"name": "one", "args": {"id": 1}}]},
                            },
                            {
                                "user_content": {"parts": [{"text": "Second turn"}]},
                                "final_response": {"parts": [{"text": "Second "}, {"text": "answer"}]},
                                "intermediate_data": {"tool_uses": []},
                            },
                        ],
                    }
                ]
            }
        ),
        encoding="utf-8",
    )
    row = mlflow_eval._load_cases(path)[0]  # noqa: SLF001 - eval parser contract
    assert row["inputs"]["turns"] == ["First turn", "Second turn"]
    assert row["expectations"]["expected_responses"] == ["First answer", "Second answer"]
    assert row["expectations"]["expected_tools"] == [[{"name": "one", "args": {"id": 1}}], []]
    assert row["tags"] == {"eval_id": "multi"}


def test_load_cases_rejects_empty_conversations_and_text(tmp_path) -> None:
    empty = tmp_path / "empty.json"
    empty.write_text(json.dumps({"eval_cases": [{"eval_id": "empty", "conversation": []}]}), encoding="utf-8")
    with pytest.raises(ValueError, match="no conversation turns"):
        mlflow_eval._load_cases(empty)  # noqa: SLF001

    no_text = tmp_path / "no-text.json"
    no_text.write_text(
        json.dumps(
            {
                "eval_cases": [
                    {
                        "eval_id": "no-text",
                        "conversation": [
                            {"user_content": {"parts": [{}]}, "final_response": {"parts": [{"text": "ok"}]}}
                        ],
                    }
                ]
            }
        ),
        encoding="utf-8",
    )
    with pytest.raises(ValueError, match="has no text parts"):
        mlflow_eval._load_cases(no_text)  # noqa: SLF001


def test_run_reuses_one_session_and_closes_runner(monkeypatch) -> None:
    runners = []

    class FakeRunner:
        def __init__(self, *, agent, app_name) -> None:
            del agent
            self.app_name = app_name
            self.session_service = SimpleNamespace(
                create_session=self.create_session,
            )
            self.calls = 0
            self.closed = False
            runners.append(self)

        async def create_session(self, **kwargs):
            assert kwargs == {"app_name": "ops-copilot", "user_id": "eval"}
            return SimpleNamespace(id="session")

        async def run_async(self, **kwargs):
            self.calls += 1
            assert kwargs["session_id"] == "session"
            call = SimpleNamespace(name=f"tool-{self.calls}", args={"turn": self.calls})
            event = SimpleNamespace(
                get_function_calls=lambda: [call],
                is_final_response=lambda: True,
                content=types.Content(
                    role="model",
                    parts=[types.Part(text=f"answer-{self.calls}"), types.Part(text="-complete")],
                ),
            )
            yield event

        async def close(self) -> None:
            self.closed = True

    monkeypatch.setattr(mlflow_eval, "InMemoryRunner", FakeRunner)
    result = asyncio.run(mlflow_eval._run(["one", "two"]))  # noqa: SLF001
    assert result == {
        "responses": ["answer-1-complete", "answer-2-complete"],
        "tools": [
            [{"name": "tool-1", "args": {"turn": 1}}],
            [{"name": "tool-2", "args": {"turn": 2}}],
        ],
    }
    assert runners[0].closed is True


def test_run_rejects_an_empty_conversation() -> None:
    with pytest.raises(ValueError, match="at least one turn"):
        asyncio.run(mlflow_eval._run([]))  # noqa: SLF001


def test_deterministic_scorers_cover_turn_boundaries() -> None:
    outputs = {"responses": ["answer"], "tools": [[{"name": "lookup", "args": {}}]]}
    expectations = {"expected_responses": ["reference"], "expected_tools": [[{"name": "lookup", "args": {}}]]}
    assert mlflow_eval.tool_trajectory(outputs=outputs, expectations=expectations) is True
    assert mlflow_eval.complete_conversation(outputs=outputs, expectations=expectations) is True
    assert mlflow_eval.complete_conversation(outputs={"responses": [""]}, expectations=expectations) is False


def test_optional_judge_requires_gateway_configuration(monkeypatch) -> None:
    monkeypatch.setenv("MLFLOW_JUDGE_MODEL", "judge")
    for name in ("MLFLOW_JUDGE_BASE_URL", "MLFLOW_JUDGE_API_KEY", "OPENAI_BASE_URL", "OPENAI_API_KEY"):
        monkeypatch.delenv(name, raising=False)
    with pytest.raises(ValueError, match="agentgateway URL/key"):
        mlflow_eval._scorers()  # noqa: SLF001


def test_optional_judge_uses_openai_sdk_through_gateway(monkeypatch) -> None:
    calls: dict[str, object] = {}

    class FakeOpenAI:
        def __init__(self, **kwargs) -> None:
            calls["client"] = kwargs
            self.chat = SimpleNamespace(completions=SimpleNamespace(create=self.create))

        def __enter__(self):
            return self

        def __exit__(self, *_args) -> None:
            return None

        def create(self, **kwargs):
            calls["request"] = kwargs
            message = SimpleNamespace(content='{"passed": true, "rationale": "Grounded in the reference."}')
            return SimpleNamespace(choices=[SimpleNamespace(message=message)])

    monkeypatch.setattr(mlflow_eval, "OpenAI", FakeOpenAI)
    judge = mlflow_eval._gateway_judge("judge-model", "http://localhost:4000/v1", "local-marker")  # noqa: SLF001
    feedback = judge(
        inputs={"turns": ["question"]},
        outputs={"responses": ["answer"]},
        expectations={"expected_responses": ["answer"]},
    )
    assert isinstance(feedback, Feedback)
    assert feedback.value is True
    assert feedback.source is not None
    assert feedback.source.source_id == "agentgateway:judge-model"
    assert calls["client"] == {"base_url": "http://localhost:4000/v1", "api_key": "local-marker"}
    request = cast("dict[str, Any]", calls["request"])
    assert request["model"] == "judge-model"


def _stub_run_context(monkeypatch, tags: dict | None = None) -> None:
    """Stub the explicit parent-run wrapper so tests never touch a tracking store."""
    from contextlib import contextmanager

    @contextmanager
    def fake_start_run(**_kwargs):
        yield SimpleNamespace(info=SimpleNamespace(run_id="run-1"))

    monkeypatch.setattr(mlflow_eval.mlflow, "start_run", fake_start_run)
    recorder = tags if tags is not None else {}
    monkeypatch.setattr(mlflow_eval.mlflow, "set_tags", recorder.update)


def test_main_links_prompt_version_to_evaluated_model(monkeypatch, capsys) -> None:
    finalized: list[tuple[str, str]] = []
    evaluated: dict[str, object] = {}
    monkeypatch.setattr(mlflow_eval.mlflow, "set_tracking_uri", lambda _uri: None)
    monkeypatch.setattr(
        mlflow_eval.mlflow,
        "set_experiment",
        lambda _name: SimpleNamespace(experiment_id="experiment-1"),
    )
    monkeypatch.setattr(
        mlflow_eval.mlflow.genai,
        "register_prompt",
        lambda **_kwargs: SimpleNamespace(
            uri="prompts:/ops-copilot-instruction/7", version=7, name="ops-copilot-instruction"
        ),
    )

    def initialize(**kwargs):
        assert kwargs["params"]["prompt_uri"] == "prompts:/ops-copilot-instruction/7"
        assert kwargs["params"]["prompt_version"] == "7"
        return SimpleNamespace(model_id="model-1")

    def evaluate(**kwargs):
        evaluated.update(kwargs)
        return SimpleNamespace(metrics={"tool_trajectory/mean": 1.0})

    monkeypatch.setattr(mlflow_eval.mlflow, "initialize_logged_model", initialize)
    monkeypatch.setattr(mlflow_eval.mlflow, "finalize_logged_model", lambda *_args: finalized.append(_args))
    monkeypatch.setattr(mlflow_eval.mlflow.genai, "evaluate", evaluate)
    monkeypatch.setattr(mlflow_eval, "_load_cases", list)
    monkeypatch.setattr(mlflow_eval, "_scorers", list)
    tags: dict = {}
    _stub_run_context(monkeypatch, tags)
    mlflow_eval.main()
    assert tags == {"prompt_name": "ops-copilot-instruction", "prompt_version": "7"}
    assert evaluated["model_id"] == "model-1"
    assert finalized == [("model-1", "READY")]
    output = capsys.readouterr().out
    assert "MLflow eval complete" in output
    assert f"Tracking URI: {mlflow_eval._TRACKING_URI}" in output  # noqa: SLF001
    assert "Local UI:" in output


def test_remote_tracking_uri_does_not_print_a_local_ui_command(monkeypatch, capsys) -> None:
    monkeypatch.setattr(mlflow_eval, "_TRACKING_URI", "http://mlflow:5000")
    monkeypatch.setattr(mlflow_eval.mlflow, "set_tracking_uri", lambda _uri: None)
    monkeypatch.setattr(
        mlflow_eval.mlflow,
        "set_experiment",
        lambda _name: SimpleNamespace(experiment_id="experiment-1"),
    )
    monkeypatch.setattr(
        mlflow_eval.mlflow.genai,
        "register_prompt",
        lambda **_kwargs: SimpleNamespace(uri="prompts:/instruction/1", version=1, name="instruction"),
    )
    monkeypatch.setattr(
        mlflow_eval.mlflow,
        "initialize_logged_model",
        lambda **_kwargs: SimpleNamespace(model_id="model-1"),
    )
    monkeypatch.setattr(mlflow_eval.mlflow, "finalize_logged_model", lambda *_args: None)
    monkeypatch.setattr(
        mlflow_eval.mlflow.genai,
        "evaluate",
        lambda **_kwargs: SimpleNamespace(metrics={}),
    )
    monkeypatch.setattr(mlflow_eval, "_load_cases", list)
    monkeypatch.setattr(mlflow_eval, "_scorers", list)
    _stub_run_context(monkeypatch)
    mlflow_eval.main()
    output = capsys.readouterr().out
    assert "Tracking URI: http://mlflow:5000" in output
    assert "Local UI:" not in output


def test_main_marks_logged_model_failed_when_evaluation_fails(monkeypatch) -> None:
    finalized: list[tuple[str, str]] = []
    monkeypatch.setattr(mlflow_eval.mlflow, "set_tracking_uri", lambda _uri: None)
    monkeypatch.setattr(
        mlflow_eval.mlflow,
        "set_experiment",
        lambda _name: SimpleNamespace(experiment_id="experiment-1"),
    )
    monkeypatch.setattr(
        mlflow_eval.mlflow.genai,
        "register_prompt",
        lambda **_kwargs: SimpleNamespace(uri="prompts:/instruction/1", version=1, name="instruction"),
    )
    monkeypatch.setattr(
        mlflow_eval.mlflow,
        "initialize_logged_model",
        lambda **_kwargs: SimpleNamespace(model_id="model-1"),
    )
    monkeypatch.setattr(mlflow_eval.mlflow, "finalize_logged_model", lambda *_args: finalized.append(_args))

    def fail_evaluation(**_kwargs):
        raise RuntimeError("fail")

    monkeypatch.setattr(mlflow_eval.mlflow.genai, "evaluate", fail_evaluation)
    monkeypatch.setattr(mlflow_eval, "_load_cases", list)
    monkeypatch.setattr(mlflow_eval, "_scorers", list)
    _stub_run_context(monkeypatch)
    with pytest.raises(RuntimeError, match="fail"):
        mlflow_eval.main()
    assert finalized == [("model-1", "FAILED")]
