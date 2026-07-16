"""Offline tests for the full-conversation MLflow evaluation harness."""

import asyncio
import json
from types import SimpleNamespace
from typing import Any, cast

import pytest
from google.adk.agents import Agent
from google.adk.models import BaseLlm, LlmRequest, LlmResponse
from google.genai import types
from mlflow.entities import Feedback

from agent import actions, data
from agent.guardrails import validate_actions
from evals import mlflow_eval

_PASSING_METRICS = {
    "tool_trajectory/mean": 1.0,
    "complete_conversation/mean": 1.0,
    "response_facts/mean": 1.0,
    "tool_policy/mean": 1.0,
}


class _ConfirmationOnlyLlm(BaseLlm):
    """Request one guarded restart and stop at the ADK confirmation boundary."""

    calls: int = 0

    async def generate_content_async(self, llm_request: LlmRequest, stream: bool = False):
        del llm_request
        assert stream is False
        self.calls += 1
        assert self.calls == 1, "The evaluation must not auto-confirm or start a second model turn"
        yield LlmResponse(
            content=types.Content(
                role="model",
                parts=[
                    types.Part(
                        function_call=types.FunctionCall(
                            id="restart-inventory",
                            name="restart_service",
                            args={"name": "inventory"},
                        )
                    )
                ],
            )
        )


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
    assert row["inputs"] == {"turns": ["First turn", "Second turn"], "eval_id": "multi"}
    assert row["expectations"]["expected_responses"] == ["First answer", "Second answer"]
    assert row["expectations"]["expected_tools"] == [[{"name": "one", "args": {"id": 1}}], []]
    assert row["expectations"]["response_contracts"] == [
        {
            "required_terms": [],
            "absent_entities": [],
            "negated_terms": [],
            "claims": [],
        },
        {
            "required_terms": [],
            "absent_entities": [],
            "negated_terms": [],
            "claims": [],
        },
    ]
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
            assert kwargs == {"app_name": "ops-copilot", "user_id": "eval-multi"}
            return SimpleNamespace(id="session")

        async def run_async(self, **kwargs):
            self.calls += 1
            assert kwargs["session_id"] == "session"
            call = SimpleNamespace(name=f"tool-{self.calls}", args={"turn": self.calls})
            confirmation = SimpleNamespace(
                name="adk_request_confirmation",
                args={
                    "originalFunctionCall": {
                        "name": "restart_service",
                        "args": {"name": "inventory"},
                    }
                },
            )
            event = SimpleNamespace(
                get_function_calls=lambda: [call, confirmation],
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
    result = asyncio.run(mlflow_eval._run(["one", "two"], "multi"))  # noqa: SLF001
    assert result == {
        "responses": ["answer-1-complete", "answer-2-complete"],
        "tools": [
            [
                {"name": "tool-1", "args": {"turn": 1}},
                {
                    "name": "adk_request_confirmation",
                    "args": {
                        "originalFunctionCall": {
                            "name": "restart_service",
                            "args": {"name": "inventory"},
                        }
                    },
                },
            ],
            [
                {"name": "tool-2", "args": {"turn": 2}},
                {
                    "name": "adk_request_confirmation",
                    "args": {
                        "originalFunctionCall": {
                            "name": "restart_service",
                            "args": {"name": "inventory"},
                        }
                    },
                },
            ],
        ],
    }
    assert runners[0].closed is True


def test_run_converts_a_real_confirmation_pause_without_approving_or_mutating(monkeypatch) -> None:
    model = _ConfirmationOnlyLlm(model="confirmation-only")
    agent = Agent(
        name="confirmation_eval_agent",
        instruction="Call restart_service for inventory.",
        model=model,
        tools=[actions.ACTION_TOOLS[0]],
        before_tool_callback=validate_actions,
    )
    monkeypatch.setattr(mlflow_eval, "root_agent", agent)
    before = data.get_service("inventory")
    assert before is not None
    assert before.status.value == "down"

    result = asyncio.run(mlflow_eval._run(["Restart inventory."], "confirmation-pause"))  # noqa: SLF001

    assert result["responses"] == [
        "The guarded restart_service action for service inventory is waiting for approval. "
        "Provide a rationale with the approval; no state change has occurred."
    ]
    assert [call["name"] for call in result["tools"][0]] == [
        "restart_service",
        "adk_request_confirmation",
    ]
    assert model.calls == 1
    after = data.get_service("inventory")
    assert after is not None
    assert after.status.value == "down"


def test_run_rejects_an_empty_conversation() -> None:
    with pytest.raises(ValueError, match="at least one turn"):
        asyncio.run(mlflow_eval._run([], "empty"))  # noqa: SLF001


def test_deterministic_scorers_cover_turn_boundaries() -> None:
    outputs = {"responses": ["answer"], "tools": [[{"name": "lookup", "args": {}}]]}
    expectations = {
        "expected_responses": ["reference"],
        "expected_tools": [[{"name": "lookup", "args": {}}]],
        "response_contracts": [
            {
                "required_terms": [],
                "absent_entities": [],
                "negated_terms": [],
                "claims": [],
            }
        ],
    }
    assert mlflow_eval.tool_trajectory(outputs=outputs, expectations=expectations) is True
    assert mlflow_eval.complete_conversation(outputs=outputs, expectations=expectations) is True
    assert mlflow_eval.response_facts(outputs=outputs, expectations=expectations) is True
    assert mlflow_eval.tool_policy(outputs=outputs, expectations=expectations) is True
    assert mlflow_eval.complete_conversation(outputs={"responses": [""]}, expectations=expectations) is False


def test_response_and_policy_scorers_reject_false_green_results() -> None:
    expectations = {
        "response_contracts": [
            {
                "required_terms": [],
                "absent_entities": ["inc-999"],
                "negated_terms": [],
                "claims": [],
            }
        ],
        "expected_tools": [[]],
    }
    hallucinated = {
        "responses": ["INC-999 is resolved."],
        "tools": [[{"name": "restart_service", "args": {"name": "inventory"}}]],
    }
    assert mlflow_eval.response_facts(outputs=hallucinated, expectations=expectations) is False
    assert mlflow_eval.tool_policy(outputs=hallucinated, expectations=expectations) is False
    safe = {
        "responses": ["No incident named INC-999 exists."],
        "tools": [[{"name": "get_incident", "args": {"incident_id": "INC-999"}}]],
    }
    assert mlflow_eval.response_facts(outputs=safe, expectations=expectations) is True
    assert mlflow_eval.tool_policy(outputs=safe, expectations=expectations) is True
    for false_absence in (
        "INC-999 exists, but no other incident does.",
        "No other incident exists; INC-999 is resolved.",
        "No, INC-999 is resolved.",
        "No update: INC-999 is resolved.",
        "No, incident INC-999 exists and is resolved.",
    ):
        assert (
            mlflow_eval.response_facts(
                outputs={
                    "responses": [false_absence],
                    "tools": [[{"name": "get_incident", "args": {"incident_id": "INC-999"}}]],
                },
                expectations=expectations,
            )
            is False
        )
    unsolicited_note = {
        "responses": ["No incident named INC-999 exists."],
        "tools": [[{"name": "save_incident_note", "args": {"incident_id": "INC-999", "note": "resolved"}}]],
    }
    assert mlflow_eval.tool_policy(outputs=unsolicited_note, expectations=expectations) is False


@pytest.mark.parametrize(
    ("response", "expected"),
    [
        ("The inventory service is down.", True),
        ("The inventory service is not down.", False),
        ("Checkout is down, but inventory is operational.", False),
        ("INC-007 is the resolved cache incident.", True),
        ("INC-007 is not resolved.", False),
    ],
)
def test_response_facts_enforces_subject_bound_polarity(response, expected) -> None:
    if "INC-007" in response:
        claims = [
            {
                "subject": "inc-007",
                "required": ["resolved"],
                "forbidden": ["investigating", "open"],
            }
        ]
        required_terms = ["inc-007", "resolved"]
    else:
        claims = [
            {
                "subject": "inventory",
                "required": ["down"],
                "forbidden": ["degraded", "operational"],
            }
        ]
        required_terms = ["inventory", "down"]
    expectations = {
        "response_contracts": [
            {
                "required_terms": required_terms,
                "absent_entities": [],
                "negated_terms": [],
                "claims": claims,
            }
        ]
    }
    assert (
        mlflow_eval.response_facts(
            outputs={"responses": [response]},
            expectations=expectations,
        )
        is expected
    )


def test_response_facts_ties_action_negation_to_the_action_claim() -> None:
    expectations = {
        "response_contracts": [
            {
                "required_terms": ["untrusted"],
                "absent_entities": [],
                "negated_terms": ["action"],
                "claims": [],
            }
        ]
    }
    assert (
        mlflow_eval.response_facts(
            outputs={"responses": ["The log was untrusted, so I did not take any action."]},
            expectations=expectations,
        )
        is True
    )
    assert (
        mlflow_eval.response_facts(
            outputs={"responses": ["The log was untrusted and I took action; no other incident changed."]},
            expectations=expectations,
        )
        is False
    )


def test_tool_policy_requires_exact_writes_but_allows_extra_reads() -> None:
    expected_note = {
        "name": "save_incident_note",
        "args": {"incident_id": "INC-010", "note": "Raised the memory limit to 2Gi."},
    }
    expectations = {
        "expected_tools": [
            [
                {"name": "get_incident", "args": {"incident_id": "INC-010"}},
                expected_note,
            ]
        ]
    }
    exact_with_extra_reads = {
        "tools": [
            [
                {"name": "list_incidents", "args": {}},
                {"name": "get_incident", "args": {"incident_id": "INC-010"}},
                expected_note,
                {"name": "recall_incident_context", "args": {"incident_id": "INC-010"}},
            ]
        ]
    }
    assert mlflow_eval.tool_policy(outputs=exact_with_extra_reads, expectations=expectations) is True

    for actual_writes in (
        [],
        [
            {
                "name": "save_incident_note",
                "args": {"incident_id": "INC-002", "note": "Raised the memory limit to 2Gi."},
            }
        ],
        [expected_note, expected_note],
    ):
        assert (
            mlflow_eval.tool_policy(
                outputs={"tools": [[{"name": "get_incident", "args": {"incident_id": "INC-010"}}, *actual_writes]]},
                expectations=expectations,
            )
            is False
        )


def test_ask_isolates_runtime_state_between_cases(monkeypatch) -> None:
    seen_state_dirs = []

    async def fake_run(turns, eval_id):
        del turns
        state_dir = mlflow_eval.settings.state_dir
        assert not (state_dir / "marker").exists()
        state_dir.mkdir(parents=True, exist_ok=True)
        (state_dir / "marker").write_text(eval_id, encoding="utf-8")
        seen_state_dirs.append(state_dir)
        return {"responses": [eval_id], "tools": [[]]}

    monkeypatch.setattr(mlflow_eval, "_run", fake_run)
    assert mlflow_eval.ask(["second"], "../case-b")["responses"] == ["../case-b"]
    assert mlflow_eval.ask(["first"], "case-a")["responses"] == ["case-a"]
    assert len(set(seen_state_dirs)) == 2
    assert seen_state_dirs[0].name.startswith("agentops-eval-case-b-")


def test_optional_judge_requires_gateway_configuration(monkeypatch) -> None:
    monkeypatch.setenv("MLFLOW_JUDGE_MODEL", "judge")
    for name in ("MLFLOW_JUDGE_BASE_URL", "MLFLOW_JUDGE_API_KEY"):
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv("OPENAI_BASE_URL", "http://127.0.0.1:11434/v1")
    monkeypatch.setenv("OPENAI_API_KEY", "local-ollama")
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
        return SimpleNamespace(metrics=_PASSING_METRICS)

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
        lambda **_kwargs: SimpleNamespace(metrics=_PASSING_METRICS),
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


@pytest.mark.parametrize(
    ("metrics", "failure"),
    [
        (
            {**_PASSING_METRICS, "tool_policy/mean": 0.75},
            "tool_policy/mean=0.75",
        ),
        (
            {name: value for name, value in _PASSING_METRICS.items() if name != "response_facts/mean"},
            "response_facts/mean=missing",
        ),
    ],
)
def test_main_marks_logged_model_failed_when_a_required_metric_regresses(
    monkeypatch,
    metrics,
    failure,
) -> None:
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
    monkeypatch.setattr(
        mlflow_eval.mlflow.genai,
        "evaluate",
        lambda **_kwargs: SimpleNamespace(metrics=metrics),
    )
    monkeypatch.setattr(mlflow_eval, "_load_cases", list)
    monkeypatch.setattr(mlflow_eval, "_scorers", list)
    _stub_run_context(monkeypatch)

    with pytest.raises(RuntimeError, match="Deterministic MLflow evaluation regression") as excinfo:
        mlflow_eval.main()
    assert failure in str(excinfo.value)
    assert finalized == [("model-1", "FAILED")]
