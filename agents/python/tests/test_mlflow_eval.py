"""Offline tests for the full-conversation MLflow evaluation harness."""

import asyncio
import contextvars
import json
import os
import subprocess
import sys
import threading
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from types import SimpleNamespace
from typing import Any, cast

import pytest
from google.adk.agents import Agent
from google.adk.models import BaseLlm, LlmRequest, LlmResponse
from google.genai import types
from mlflow.entities import Feedback

from agent import actions, data
from agent.governance import AgentOpsPolicyPlugin
from agent.guardrails import validate_actions
from evals import mlflow_eval
from tests.domain import REFERENCE_DOMAIN

_CACHE_INCIDENT = REFERENCE_DOMAIN.incidents.cache_memory
_CACHE = REFERENCE_DOMAIN.services.cache
_CHECKOUT = REFERENCE_DOMAIN.services.checkout
_GATEWAY_INCIDENT = REFERENCE_DOMAIN.incidents.gateway_memory
_INVENTORY = REFERENCE_DOMAIN.services.inventory
_INVENTORY_INCIDENT = REFERENCE_DOMAIN.incidents.inventory_down

_PASSING_METRICS = {
    "provider_available/mean": 1.0,
    "tool_trajectory/mean": 1.0,
    "complete_conversation/mean": 1.0,
    "response_facts/mean": 1.0,
    "tool_policy/mean": 1.0,
}


def _create_shadow_agent_package(package: Path) -> None:
    """Create a focused agent fake while retaining production-owned leaf modules."""
    package.mkdir()
    production_package = Path(__file__).parents[1] / "src" / "agent"
    package.joinpath("__init__.py").write_text(
        f"__path__.append({str(production_package)!r})\n",
        encoding="utf-8",
    )


@pytest.fixture(autouse=True)
def isolate_model_observations(monkeypatch, tmp_path) -> Path:
    """Keep model-backed transcript output out of the source tree in unit tests."""
    path = tmp_path / "model-observed.json"
    monkeypatch.setattr(mlflow_eval, "_MODEL_OBSERVED", path)
    return path


def test_tracking_uri_is_selected_before_composition_import_without_env(tmp_path) -> None:
    """A pinned child must see the evaluator's local store while building its agent."""
    package = tmp_path / "agent"
    _create_shadow_agent_package(package)
    (package / "config.py").write_text("settings = object()\n", encoding="utf-8")
    (package / "model.py").write_text("async def close_model(_model): pass\n", encoding="utf-8")
    (package / "composition.py").write_text(
        "\n".join(
            [
                "import os",
                "import mlflow",
                "assert mlflow.get_tracking_uri() == os.environ['EXPECTED_TRACKING_URI']",
                "INSTRUCTION = 'test instruction'",
                "root_agent = object()",
                "build_conversational_agent = lambda: root_agent",
                "build_app = lambda selected_root: selected_root",
            ]
        )
        + "\n",
        encoding="utf-8",
    )
    expected = f"sqlite:///{Path(mlflow_eval.__file__).parent / 'mlflow.db'}"
    environment = os.environ.copy()
    environment.pop("MLFLOW_TRACKING_URI", None)
    environment["EXPECTED_TRACKING_URI"] = expected
    python_paths = [str(tmp_path), str(Path(__file__).parents[1])]
    if existing := environment.get("PYTHONPATH"):
        python_paths.append(existing)
    environment["PYTHONPATH"] = os.pathsep.join(python_paths)

    completed = subprocess.run(
        [
            sys.executable,
            "-c",
            "import mlflow; import evals.mlflow_eval; print(mlflow.get_tracking_uri())",
        ],
        cwd=tmp_path,
        env=environment,
        capture_output=True,
        text=True,
        check=True,
    )

    assert completed.stdout.splitlines()[-1] == expected


def test_temp_store_prompt_can_be_registered_then_loaded_by_a_pinned_child(tmp_path) -> None:
    """The registry URI used to register a prompt is available during child import."""
    package = tmp_path / "agent"
    _create_shadow_agent_package(package)
    (package / "model.py").write_text("async def close_model(_model): pass\n", encoding="utf-8")
    (package / "config.py").write_text(
        "\n".join(
            [
                "import os",
                "from types import SimpleNamespace",
                "settings = SimpleNamespace(prompt_uri=os.environ.get('AGENT_PROMPT_URI'))",
            ]
        )
        + "\n",
        encoding="utf-8",
    )
    (package / "composition.py").write_text(
        "\n".join(
            [
                "import os",
                "from types import SimpleNamespace",
                "import mlflow.genai",
                "INSTRUCTION = 'registered probe instruction'",
                "uri = os.environ.get('AGENT_PROMPT_URI')",
                "instruction = mlflow.genai.load_prompt(uri).template if uri else INSTRUCTION",
                "root_agent = SimpleNamespace(instruction=instruction)",
                "build_conversational_agent = lambda: root_agent",
                "build_app = lambda selected_root: selected_root",
            ]
        )
        + "\n",
        encoding="utf-8",
    )
    environment = os.environ.copy()
    environment["MLFLOW_TRACKING_URI"] = f"sqlite:///{tmp_path / 'tracking.db'}"
    environment.pop("AGENT_PROMPT_URI", None)
    python_paths = [str(tmp_path), str(Path(__file__).parents[1])]
    if existing := environment.get("PYTHONPATH"):
        python_paths.append(existing)
    environment["PYTHONPATH"] = os.pathsep.join(python_paths)

    registered = subprocess.run(
        [
            sys.executable,
            "-c",
            "from evals.mlflow_eval import _evaluation_prompt; print(_evaluation_prompt().uri)",
        ],
        cwd=tmp_path,
        env=environment,
        capture_output=True,
        text=True,
        check=True,
    )
    prompt_uri = registered.stdout.splitlines()[-1]
    assert prompt_uri.startswith(f"prompts:/{mlflow_eval._PROMPT_NAME}/")  # noqa: SLF001

    environment["AGENT_PROMPT_URI"] = prompt_uri
    pinned = subprocess.run(
        [
            sys.executable,
            "-c",
            "from evals.mlflow_eval import root_agent; print(root_agent.instruction)",
        ],
        cwd=tmp_path,
        env=environment,
        capture_output=True,
        text=True,
        check=True,
    )

    assert pinned.stdout.splitlines()[-1] == "registered probe instruction"


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
                            args={"name": _INVENTORY},
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
        def __init__(self, *, app) -> None:
            self.app = app
            self.app_name = app.name
            self.session_service = SimpleNamespace(
                create_session=self.create_session,
            )
            self.calls = 0
            self.closed = False
            runners.append(self)

        async def create_session(self, **kwargs):
            assert kwargs == {"app_name": "agentops-agent", "user_id": "eval-multi"}
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
                        "args": {"name": _INVENTORY},
                    }
                },
            )
            response = SimpleNamespace(name=f"tool-{self.calls}", response={"turn": self.calls})
            event = SimpleNamespace(
                error_code="MODEL_UNAVAILABLE" if self.calls == 2 else None,
                error_message="Model request failed safely." if self.calls == 2 else None,
                get_function_calls=lambda: [call, confirmation],
                get_function_responses=lambda: [response],
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
    # Events carry no usage_metadata here, so the usage totals stay at zero.
    assert result["usage"] == {"input_tokens": 0, "output_tokens": 0, "total_tokens": 0, "model_calls": 0}
    assert result["provider_errors"] == [
        [],
        [{"code": "MODEL_UNAVAILABLE", "message": "Model request failed safely."}],
    ]
    assert {"responses": result["responses"], "tools": result["tools"]} == {
        "responses": ["answer-1-complete", "answer-2-complete"],
        "tools": [
            [
                {"name": "tool-1", "args": {"turn": 1}},
                {
                    "name": "adk_request_confirmation",
                    "args": {
                        "originalFunctionCall": {
                            "name": "restart_service",
                            "args": {"name": _INVENTORY},
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
                            "args": {"name": _INVENTORY},
                        }
                    },
                },
            ],
        ],
    }
    assert runners[0].closed is True
    assert len(runners[0].app.plugins) == 1
    assert isinstance(runners[0].app.plugins[0], AgentOpsPolicyPlugin)


def test_run_converts_a_real_confirmation_pause_without_approving_or_mutating(monkeypatch) -> None:
    model = _ConfirmationOnlyLlm(model="confirmation-only")
    agent = Agent(
        name="confirmation_eval_agent",
        instruction=f"Call restart_service for {_INVENTORY}.",
        model=model,
        tools=[actions.ACTION_TOOLS[0]],
        before_tool_callback=validate_actions,
    )
    monkeypatch.setattr(mlflow_eval, "root_agent", agent)
    before = data.get_service(_INVENTORY)
    assert before is not None
    assert before.status.value == "down"

    result = asyncio.run(mlflow_eval._run([f"Restart {_INVENTORY}."], "confirmation-pause"))  # noqa: SLF001

    assert result["responses"] == [
        (
            f"The guarded restart_service action for service {_INVENTORY} is waiting for approval. "
            "Provide a rationale with the approval; no state change has occurred."
        )
    ]
    assert [call["name"] for call in result["tools"][0]] == [
        "restart_service",
        "adk_request_confirmation",
    ]
    assert model.calls == 1
    after = data.get_service(_INVENTORY)
    assert after is not None
    assert after.status.value == "down"


def test_run_rejects_an_empty_conversation() -> None:
    with pytest.raises(ValueError, match="at least one turn"):
        asyncio.run(mlflow_eval._run([], "empty"))  # noqa: SLF001


def test_run_accumulates_token_and_call_usage(monkeypatch) -> None:
    class FakeRunner:
        def __init__(self, *, app) -> None:
            self.app_name = app.name
            self.session_service = SimpleNamespace(create_session=self._session)

        async def _session(self, **_kwargs):
            return SimpleNamespace(id="s")

        async def run_async(self, **_kwargs):
            yield SimpleNamespace(
                usage_metadata=SimpleNamespace(prompt_token_count=100, candidates_token_count=20),
                get_function_calls=list,
                get_function_responses=list,
                is_final_response=lambda: True,
                content=types.Content(role="model", parts=[types.Part(text="ok")]),
            )

        async def close(self) -> None:
            return None

    monkeypatch.setattr(mlflow_eval, "InMemoryRunner", FakeRunner)
    result = asyncio.run(mlflow_eval._run(["a", "b"], "usage"))  # noqa: SLF001
    # Two turns, one 100/20 model response each → summed over the conversation.
    assert result["usage"] == {"input_tokens": 200, "output_tokens": 40, "total_tokens": 240, "model_calls": 2}


def test_deterministic_scorers_cover_turn_boundaries() -> None:
    outputs = {
        "responses": ["answer"],
        "tools": [[{"name": "lookup", "args": {}}]],
        "provider_errors": [[]],
    }
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
    assert mlflow_eval.provider_available(outputs=outputs, expectations=expectations) is True
    assert (
        mlflow_eval.provider_available(
            outputs={**outputs, "provider_errors": []},
            expectations=expectations,
        )
        is False
    )
    assert mlflow_eval.complete_conversation(outputs=outputs, expectations=expectations) is True
    assert mlflow_eval.response_facts(outputs=outputs, expectations=expectations) is True
    assert mlflow_eval.tool_policy(outputs=outputs, expectations=expectations) is True
    assert mlflow_eval.complete_conversation(outputs={"responses": [""]}, expectations=expectations) is False
    assert (
        mlflow_eval.provider_available(
            outputs={"provider_errors": [[{"code": "MODEL_UNAVAILABLE", "message": "failed"}]]}
        )
        is False
    )


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
        "tools": [[{"name": "restart_service", "args": {"name": _INVENTORY}}]],
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
        (f"The {_INVENTORY} service is down.", True),
        (f"The {_INVENTORY} service is not down.", False),
        (f"{_CHECKOUT.title()} is down, but {_INVENTORY} is operational.", False),
        (f"{_CACHE_INCIDENT} is the resolved {_CACHE} incident.", True),
        (f"{_CACHE_INCIDENT} is not resolved.", False),
    ],
)
def test_response_facts_enforces_subject_bound_polarity(response, expected) -> None:
    if _CACHE_INCIDENT in response:
        claims = [
            {
                "subject": _CACHE_INCIDENT.lower(),
                "required": ["resolved"],
                "forbidden": ["investigating", "open"],
            }
        ]
        required_terms = [_CACHE_INCIDENT.lower(), "resolved"]
    else:
        claims = [
            {
                "subject": _INVENTORY,
                "required": ["down"],
                "forbidden": ["degraded", "operational"],
            }
        ]
        required_terms = [_INVENTORY, "down"]
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
        "args": {"incident_id": _GATEWAY_INCIDENT, "note": "Raised the memory limit to 2Gi."},
    }
    expectations = {
        "expected_tools": [
            [
                {"name": "get_incident", "args": {"incident_id": _GATEWAY_INCIDENT}},
                expected_note,
            ]
        ]
    }
    exact_with_extra_reads = {
        "tools": [
            [
                {"name": "list_incidents", "args": {}},
                {"name": "get_incident", "args": {"incident_id": _GATEWAY_INCIDENT}},
                expected_note,
                {"name": "recall_incident_context", "args": {"incident_id": _GATEWAY_INCIDENT}},
            ]
        ]
    }
    assert mlflow_eval.tool_policy(outputs=exact_with_extra_reads, expectations=expectations) is True

    for actual_writes in (
        [],
        [
            {
                "name": "save_incident_note",
                "args": {"incident_id": _INVENTORY_INCIDENT, "note": "Raised the memory limit to 2Gi."},
            }
        ],
        [expected_note, expected_note],
    ):
        assert (
            mlflow_eval.tool_policy(
                outputs={
                    "tools": [[{"name": "get_incident", "args": {"incident_id": _GATEWAY_INCIDENT}}, *actual_writes]]
                },
                expectations=expectations,
            )
            is False
        )


def test_ask_isolates_runtime_state_between_cases(monkeypatch) -> None:
    seen_state_dirs = []
    seen_event_loops = []
    evaluation_threads = []
    caller_threads = []
    closed_models = []
    fresh_agents = []
    marker = contextvars.ContextVar("eval_case_marker")

    async def fake_run(turns, eval_id, evaluation_agent):
        del turns
        seen_event_loops.append(asyncio.get_running_loop())
        evaluation_threads.append(threading.get_ident())
        fresh_agents.append(evaluation_agent)
        state_dir = mlflow_eval.settings.state_dir
        assert not (state_dir / "marker").exists()
        state_dir.mkdir(parents=True, exist_ok=True)
        (state_dir / "marker").write_text(eval_id, encoding="utf-8")
        seen_state_dirs.append(state_dir)
        return {"responses": [f"{eval_id}:{marker.get()}"], "tools": [[]]}

    async def fake_close_model(model):
        closed_models.append((model, asyncio.get_running_loop(), threading.get_ident()))

    built_agents = [SimpleNamespace(model=object()), SimpleNamespace(model=object())]
    monkeypatch.setattr(mlflow_eval, "_run", fake_run)
    monkeypatch.setattr(mlflow_eval, "close_model", fake_close_model)
    monkeypatch.setattr(mlflow_eval, "build_conversational_agent", lambda: built_agents.pop(0))
    caller_threads.append(threading.get_ident())
    marker.set("main-call")
    assert mlflow_eval.ask(["second"], "../case-b")["responses"] == ["../case-b:main-call"]

    def worker_call():
        caller_threads.append(threading.get_ident())
        marker.set("worker-call")
        return mlflow_eval.ask(["first"], "case-a")

    with ThreadPoolExecutor(max_workers=1) as executor:
        assert executor.submit(worker_call).result()["responses"] == ["case-a:worker-call"]
    assert len(set(seen_state_dirs)) == 2
    assert seen_state_dirs[0].name.startswith("agentops-eval-case-b-")
    assert seen_event_loops[0] is not seen_event_loops[1]
    assert evaluation_threads == caller_threads
    assert len({id(agent) for agent in fresh_agents}) == 2
    assert [model for model, _, _ in closed_models] == [agent.model for agent in fresh_agents]
    assert [loop for _, loop, _ in closed_models] == seen_event_loops
    assert [thread_id for _, _, thread_id in closed_models] == evaluation_threads


def test_provider_error_messages_reject_malformed_evidence() -> None:
    assert mlflow_eval.provider_error_messages({}) == ["provider error evidence is missing"]
    assert mlflow_eval.provider_error_messages({"provider_errors": "broken"}) == [
        "provider error evidence is malformed"
    ]


def test_mlflow_trace_validation_does_not_make_an_extra_prediction(monkeypatch) -> None:
    from mlflow.genai.utils.trace_utils import convert_predict_fn

    calls: list[dict] = []
    sample = {"turns": ["status?"], "eval_id": "lookup"}

    def predict(**inputs):
        calls.append(inputs)
        return {"responses": ["ok"]}

    monkeypatch.setenv(mlflow_eval._SKIP_TRACE_VALIDATION, "false")  # noqa: SLF001
    with mlflow_eval._without_mlflow_prediction_probe():  # noqa: SLF001
        converted = convert_predict_fn(predict, sample)
        assert calls == []
        assert converted(sample) == {"responses": ["ok"]}

    assert calls == [sample]
    assert os.environ[mlflow_eval._SKIP_TRACE_VALIDATION] == "false"  # noqa: SLF001


def test_model_observations_require_exact_identity_and_case_set(monkeypatch, tmp_path) -> None:
    path = tmp_path / "observed.json"
    monkeypatch.setattr(mlflow_eval.settings, "model", "qwen3:4b-instruct")
    monkeypatch.setenv("GITHUB_SHA", "abc123")
    expected_cases = [{"inputs": {"eval_id": "lookup", "turns": ["status?"]}}]
    document = {
        "schema_version": 1,
        "model_provider": str(mlflow_eval.settings.model_provider),
        "model": "qwen3:4b-instruct",
        "model_digest": "sha256:canonical",
        "prompt_selection": mlflow_eval._prompt_selection(),  # noqa: SLF001
        "resolved_prompt_uri": "prompts:/agentops-agent-instruction/7",
        "evaluation_contract_digest": mlflow_eval._evaluation_contract_digest(expected_cases),  # noqa: SLF001
        "source_revision": "abc123",
        "cases": {"lookup": {"responses": ["ok"]}},
    }
    path.write_text(json.dumps(document), encoding="utf-8")

    assert (
        mlflow_eval.load_model_observations(
            path,
            expected_cases=expected_cases,
            model_digest="sha256:canonical",
        )
        == document["cases"]
    )
    with pytest.raises(
        SystemExit, match="does not match the configured model, prompt, source revision, or eval contract"
    ):
        mlflow_eval.load_model_observations(
            path,
            expected_cases=expected_cases,
            model_digest="sha256:different",
        )
    with pytest.raises(
        SystemExit, match="does not match the configured model, prompt, source revision, or eval contract"
    ):
        mlflow_eval.load_model_observations(
            path,
            expected_cases=[{"inputs": {"eval_id": "another-case", "turns": ["different"]}}],
            model_digest="sha256:canonical",
        )
    monkeypatch.delenv("GITHUB_SHA")
    with pytest.raises(SystemExit, match="requires non-empty EVAL_MODEL_DIGEST and GITHUB_SHA"):
        mlflow_eval.load_model_observations(
            path,
            expected_cases=expected_cases,
            model_digest="sha256:canonical",
        )


@pytest.mark.parametrize(
    ("configured_name", "configured_value"),
    [
        ("MLFLOW_JUDGE_MODEL", "judge"),
        ("MLFLOW_JUDGE_BASE_URL", "http://localhost:4000/v1"),
        ("MLFLOW_JUDGE_API_KEY", "marker"),
    ],
)
def test_optional_judge_requires_complete_gateway_configuration(
    monkeypatch,
    configured_name: str,
    configured_value: str,
) -> None:
    for name in ("MLFLOW_JUDGE_MODEL", "MLFLOW_JUDGE_BASE_URL", "MLFLOW_JUDGE_API_KEY"):
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv(configured_name, configured_value)
    monkeypatch.setenv("OPENAI_BASE_URL", "http://127.0.0.1:11434/v1")
    monkeypatch.setenv("OPENAI_API_KEY", "local-ollama")
    with pytest.raises(ValueError, match="must be set together"):
        mlflow_eval._scorers()  # noqa: SLF001


def test_min_score_override_can_raise_but_never_weaken_defaults(monkeypatch) -> None:
    monkeypatch.setenv("AGENT_EVAL_MIN_SCORE", "0.8")
    assert mlflow_eval._min_scores() == {  # noqa: SLF001
        "provider_available/mean": 1.0,
        "tool_trajectory/mean": 0.8,
        "complete_conversation/mean": 1.0,
        "response_facts/mean": 0.8,
        "tool_policy/mean": 0.8,
    }
    monkeypatch.setenv("AGENT_EVAL_MIN_SCORE", "0.1")
    assert mlflow_eval._min_scores() == mlflow_eval._DEFAULT_MIN_SCORES  # noqa: SLF001


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


def _stub_run_context(
    monkeypatch,
    tags: dict | None = None,
    prompt_links: list[tuple[str, dict[str, object]]] | None = None,
) -> None:
    """Stub the explicit parent-run wrapper so tests never touch a tracking store."""
    from contextlib import contextmanager

    @contextmanager
    def fake_start_run(**_kwargs):
        yield SimpleNamespace(info=SimpleNamespace(run_id="run-1"))

    monkeypatch.setattr(mlflow_eval.mlflow, "start_run", fake_start_run)
    recorder = tags if tags is not None else {}
    monkeypatch.setattr(mlflow_eval.mlflow, "set_tags", recorder.update)
    monkeypatch.setattr(mlflow_eval, "_matching_registered_prompt", lambda _template: None)
    links = prompt_links if prompt_links is not None else []
    monkeypatch.setattr(
        mlflow_eval,
        "MlflowClient",
        lambda: SimpleNamespace(
            link_prompt_version_to_model=lambda **kwargs: links.append(("model", kwargs)),
            link_prompt_version_to_run=lambda **kwargs: links.append(("run", kwargs)),
        ),
    )


def test_evaluation_prompt_reuses_the_configured_registry_version(monkeypatch) -> None:
    expected = SimpleNamespace(
        uri="prompts:/agentops-agent-instruction/3",
        version=3,
        name="agentops-agent-instruction",
    )
    monkeypatch.setattr(mlflow_eval, "settings", SimpleNamespace(prompt_uri=expected.uri))
    monkeypatch.setattr(mlflow_eval.mlflow.genai, "load_prompt", lambda uri: expected if uri == expected.uri else None)
    monkeypatch.setattr(
        mlflow_eval.mlflow.genai,
        "register_prompt",
        lambda **_kwargs: pytest.fail("a pinned registry prompt must not be relabeled as the committed prompt"),
    )

    assert mlflow_eval._evaluation_prompt() is expected  # noqa: SLF001 - prompt lineage contract


def test_evaluation_prompt_rejects_mutable_aliases(monkeypatch) -> None:
    monkeypatch.setattr(
        mlflow_eval,
        "settings",
        SimpleNamespace(prompt_uri="prompts:/agentops-agent-instruction@latest"),
    )
    with pytest.raises(SystemExit, match="immutable numeric prompt version"):
        mlflow_eval._evaluation_prompt()  # noqa: SLF001


def test_matching_registered_prompt_reuses_the_latest_identical_template(monkeypatch) -> None:
    latest = SimpleNamespace(template=mlflow_eval.INSTRUCTION, version=7)
    monkeypatch.setattr(
        mlflow_eval.mlflow.genai,
        "load_prompt",
        lambda *_args, **_kwargs: latest,
    )
    monkeypatch.setattr(
        mlflow_eval,
        "MlflowClient",
        lambda: pytest.fail("the latest identical prompt needs no history search"),
    )

    assert mlflow_eval._matching_registered_prompt(mlflow_eval.INSTRUCTION) is latest  # noqa: SLF001


def test_matching_registered_prompt_searches_every_historical_page(monkeypatch) -> None:
    latest = SimpleNamespace(template="newer text", version=4)
    historical = SimpleNamespace(template=mlflow_eval.INSTRUCTION, version=1)
    calls: list[tuple[str, int, str | None]] = []

    class Page(list):
        def __init__(self, values, token) -> None:
            super().__init__(values)
            self.token = token

    def search(name, *, max_results, page_token):
        calls.append((name, max_results, page_token))
        if page_token is None:
            return Page([latest], "next")
        return Page([historical], None)

    monkeypatch.setattr(
        mlflow_eval.mlflow.genai,
        "load_prompt",
        lambda *_args, **_kwargs: latest,
    )
    monkeypatch.setattr(
        mlflow_eval,
        "MlflowClient",
        lambda: SimpleNamespace(search_prompt_versions=search),
    )

    assert mlflow_eval._matching_registered_prompt(mlflow_eval.INSTRUCTION) is historical  # noqa: SLF001
    assert calls == [
        (mlflow_eval._PROMPT_NAME, mlflow_eval._PROMPT_PAGE_SIZE, None),  # noqa: SLF001
        (mlflow_eval._PROMPT_NAME, mlflow_eval._PROMPT_PAGE_SIZE, "next"),  # noqa: SLF001
    ]


def test_evaluation_prompt_reuses_a_matching_historical_version(monkeypatch) -> None:
    historical = SimpleNamespace(template=mlflow_eval.INSTRUCTION, version=2)
    monkeypatch.setattr(mlflow_eval, "settings", SimpleNamespace(prompt_uri=None))
    monkeypatch.setattr(
        mlflow_eval,
        "_matching_registered_prompt",
        lambda template: historical if template == mlflow_eval.INSTRUCTION else None,
    )
    monkeypatch.setattr(
        mlflow_eval.mlflow.genai,
        "register_prompt",
        lambda **_kwargs: pytest.fail("identical historical text must reuse its prompt version"),
    )

    assert mlflow_eval._evaluation_prompt() is historical  # noqa: SLF001


def test_evaluation_prompt_registers_only_when_no_template_matches(monkeypatch) -> None:
    registered = SimpleNamespace(template=mlflow_eval.INSTRUCTION, version=8)
    monkeypatch.setattr(mlflow_eval, "settings", SimpleNamespace(prompt_uri=None))
    monkeypatch.setattr(mlflow_eval, "_matching_registered_prompt", lambda _template: None)
    monkeypatch.setattr(
        mlflow_eval.mlflow.genai,
        "register_prompt",
        lambda **kwargs: (
            registered
            if kwargs
            == {
                "name": mlflow_eval._PROMPT_NAME,  # noqa: SLF001
                "template": mlflow_eval.INSTRUCTION,
                "commit_message": "AgentOps Agent system instruction",
            }
            else pytest.fail(f"unexpected registration: {kwargs}")
        ),
    )

    assert mlflow_eval._evaluation_prompt() is registered  # noqa: SLF001


def test_main_links_prompt_version_to_evaluated_model_and_parent_run(
    monkeypatch,
    capsys,
    isolate_model_observations,
) -> None:
    finalized: list[tuple[str, str]] = []
    evaluated: dict[str, object] = {}
    prompt_links: list[tuple[str, dict[str, object]]] = []
    registered_prompt = SimpleNamespace(
        uri="prompts:/agentops-agent-instruction/7",
        version=7,
        name="agentops-agent-instruction",
    )
    monkeypatch.setenv("EVAL_MODEL_DIGEST", "sha256:canonical")
    monkeypatch.setenv(mlflow_eval._SKIP_TRACE_VALIDATION, "false")  # noqa: SLF001
    monkeypatch.setattr(mlflow_eval.mlflow, "set_tracking_uri", lambda _uri: None)
    monkeypatch.setattr(
        mlflow_eval.mlflow,
        "set_experiment",
        lambda _name: SimpleNamespace(experiment_id="experiment-1"),
    )
    monkeypatch.setattr(
        mlflow_eval.mlflow.genai,
        "register_prompt",
        lambda **_kwargs: registered_prompt,
    )

    def initialize(**kwargs):
        assert kwargs["params"]["agent_model_provider"] == "openai-compatible"
        assert kwargs["params"]["agent_model_digest"] == "sha256:canonical"
        assert kwargs["params"]["prompt_uri"] == "prompts:/agentops-agent-instruction/7"
        assert kwargs["params"]["prompt_version"] == "7"
        return SimpleNamespace(model_id="model-1")

    def evaluate(**kwargs):
        assert os.environ[mlflow_eval._SKIP_TRACE_VALIDATION] == "true"  # noqa: SLF001
        evaluated.update(kwargs)
        kwargs["predict_fn"](["status?"], "lookup")
        return SimpleNamespace(metrics=_PASSING_METRICS)

    monkeypatch.setattr(mlflow_eval.mlflow, "initialize_logged_model", initialize)
    monkeypatch.setattr(mlflow_eval.mlflow, "finalize_logged_model", lambda *_args: finalized.append(_args))
    monkeypatch.setattr(mlflow_eval.mlflow.genai, "evaluate", evaluate)
    monkeypatch.setattr(
        mlflow_eval,
        "_load_cases",
        lambda: [{"inputs": {"eval_id": "lookup", "turns": ["status?"]}}],
    )
    monkeypatch.setattr(
        mlflow_eval,
        "ask",
        lambda turns, eval_id: {
            "responses": [f"{eval_id}:{turns[0]}"],
            "tools": [[]],
            "usage": {"total_tokens": 10, "model_calls": 1},
            "evidence": [""],
            "provider_errors": [[]],
        },
    )
    monkeypatch.setattr(mlflow_eval, "_scorers", list)
    tags: dict = {}
    _stub_run_context(monkeypatch, tags, prompt_links)
    mlflow_eval.main()
    assert tags == {"prompt_name": "agentops-agent-instruction", "prompt_version": "7"}
    prompt = prompt_links[1][1]["prompt"]
    assert prompt_links == [
        (
            "model",
            {
                "name": "agentops-agent-instruction",
                "version": "7",
                "model_id": "model-1",
            },
        ),
        ("run", {"run_id": "run-1", "prompt": prompt}),
    ]
    assert prompt is registered_prompt
    assert evaluated["model_id"] == "model-1"
    observed = json.loads(isolate_model_observations.read_text(encoding="utf-8"))
    assert observed["model_digest"] == "sha256:canonical"
    assert observed["prompt_selection"].startswith("committed:sha256:")
    assert observed["resolved_prompt_uri"] == "prompts:/agentops-agent-instruction/7"
    assert observed["evaluation_contract_digest"]
    assert observed["cases"]["lookup"]["responses"] == ["lookup:status?"]
    assert finalized == [("model-1", "READY")]
    assert os.environ[mlflow_eval._SKIP_TRACE_VALIDATION] == "false"  # noqa: SLF001
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


def test_main_marks_logged_model_failed_when_evaluation_fails(
    monkeypatch,
    isolate_model_observations,
) -> None:
    finalized: list[tuple[str, str]] = []
    isolate_model_observations.write_text("stale", encoding="utf-8")
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
    assert not isolate_model_observations.exists()


@pytest.mark.parametrize(
    ("metrics", "failure"),
    [
        (
            # Below the 0.60 tool_policy floor: the agent is proposing the wrong guarded write
            # in more than a third of turns, which is a collapse rather than a weak model.
            {**_PASSING_METRICS, "tool_policy/mean": 0.5},
            "tool_policy/mean=0.5",
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
