"""Offline contract tests for the tracked A2A server."""

import asyncio
import json
from typing import Any, cast

from a2a.server.agent_execution import RequestContext
from google.adk.a2a.converters.part_converter import A2APartToGenAIPartConverter
from google.adk.a2a.converters.request_converter import AgentRunRequest
from google.adk.agents import Agent, RunConfig
from google.adk.models import BaseLlm, LlmRequest, LlmResponse
from google.genai import types
from sqlalchemy.ext.asyncio import create_async_engine
from starlette.testclient import TestClient

from agent import server
from agent.config import settings
from agent.guardrails import handle_model_error


class _StreamingLlm(BaseLlm):
    """Deterministic model double that emits two chunks or interrupts after one."""

    fail_after_first: bool = False

    async def generate_content_async(self, llm_request: LlmRequest, stream: bool = False):
        del llm_request
        assert stream is True
        yield LlmResponse(
            content=types.Content(role="model", parts=[types.Part(text="first ")]),
            partial=True,
        )
        if self.fail_after_first:
            raise RuntimeError("stream interrupted")
        yield LlmResponse(
            content=types.Content(role="model", parts=[types.Part(text="first second")]),
            partial=False,
        )


def _stream_events(*, fail_after_first: bool) -> list[dict[str, Any]]:
    agent = Agent(
        name="stream_test_agent",
        instruction="Reply using the fake model.",
        model=_StreamingLlm(model="stream-test", fail_after_first=fail_after_first),
        on_model_error_callback=handle_model_error,
    )
    payload = {
        "jsonrpc": "2.0",
        "id": "stream-test",
        "method": "message/stream",
        "params": {
            "message": {
                "kind": "message",
                "messageId": "stream-message",
                "role": "user",
                "parts": [{"kind": "text", "text": "Stream a reply."}],
            }
        },
    }
    with TestClient(server.create_app(agent)) as client, client.stream("POST", "/", json=payload) as response:
        assert response.status_code == 200
        assert response.headers["content-type"].startswith("text/event-stream")
        return [json.loads(line.removeprefix("data: ")) for line in response.iter_lines() if line.startswith("data: ")]


def test_agent_card_is_public_and_does_not_expose_instruction() -> None:
    assert server.agent_card.url == "http://localhost:8080/"
    assert server.agent_card.version == "0.1.0"
    assert "Operating rules" not in server.agent_card.description
    assert {skill.id for skill in server.agent_card.skills} == {"incident-triage", "remediation"}


def test_app_factory_owns_and_closes_persistent_runtime() -> None:
    async def exercise_lifespan() -> None:
        app = server.create_app()
        runtime = app.state.runtime
        assert runtime.runner.session_service is runtime.session_service
        assert runtime.session_service.__class__.__name__ == "DatabaseSessionService"
        assert runtime.task_store.__class__.__name__ == "DatabaseTaskStore"

        async with app.router.lifespan_context(app):
            await runtime.session_service.create_session(app_name="ops-copilot", user_id="test")
            await runtime.task_store.initialize()

    asyncio.run(exercise_lifespan())


def test_main_runs_uvicorn_with_an_app_factory(monkeypatch) -> None:
    call: dict[str, object] = {}

    def fake_run(app, **kwargs) -> None:
        call.update({"app": app, **kwargs})

    monkeypatch.setattr(server.uvicorn, "run", fake_run)
    server.main()
    assert call == {
        "app": server.create_app,
        "factory": True,
        "host": "0.0.0.0",  # noqa: S104 - verifies the intentional container listener
        "port": 8080,
        "timeout_graceful_shutdown": 10,  # bounded SIGTERM drain (Ch. 6)
    }


def test_health_endpoints_report_ready() -> None:
    with TestClient(server.create_app()) as client:
        alive = client.get("/livez")
        assert alive.status_code == 200
        assert alive.json() == {"status": "alive"}
        ready = client.get("/healthz")
        assert ready.status_code == 200
        assert ready.json() == {"status": "ready"}


def test_readiness_fails_without_the_seed_dataset(tmp_path, monkeypatch) -> None:
    with TestClient(server.create_app()) as client:
        monkeypatch.setattr(settings, "data_dir", tmp_path / "missing")
        monkeypatch.setattr(settings, "state_dir", tmp_path / "fresh-state")
        response = client.get("/healthz")
        assert response.status_code == 503
        assert any("dataset unavailable" in problem for problem in response.json()["problems"])


def test_readiness_fails_when_the_session_store_is_unreachable(tmp_path) -> None:
    app = server.create_app()
    with TestClient(app) as client:
        runtime = app.state.runtime
        broken = create_async_engine(f"sqlite+aiosqlite:///{tmp_path}/no-such-dir/runtime.db")
        healthy_engine = runtime.task_engine
        runtime.task_engine = broken
        try:
            response = client.get("/healthz")
        finally:
            runtime.task_engine = healthy_engine
        assert response.status_code == 503
        assert any("session store unreachable" in problem for problem in response.json()["problems"])


def test_a2a_requests_have_a_bounded_model_call_budget(monkeypatch) -> None:
    def fake_converter(request, part_converter) -> AgentRunRequest:
        del request, part_converter
        return AgentRunRequest(run_config=RunConfig(max_llm_calls=500, custom_metadata={"source": "a2a"}))

    monkeypatch.setattr(server, "convert_a2a_request_to_agent_run_request", fake_converter)
    converted = server._bounded_request(  # noqa: SLF001 - request policy contract
        cast("RequestContext", None),
        cast("A2APartToGenAIPartConverter", None),
    )
    assert converted.run_config is not None
    assert converted.run_config.max_llm_calls == 12
    assert converted.run_config.custom_metadata == {"source": "a2a"}
    # Model streaming stays off unless explicitly opted into (Ch. 3.6).
    assert converted.run_config.streaming_mode is server.StreamingMode.NONE


def test_a2a_streaming_is_an_explicit_opt_in(monkeypatch) -> None:
    def fake_converter(request, part_converter) -> AgentRunRequest:
        del request, part_converter
        return AgentRunRequest(run_config=RunConfig())

    monkeypatch.setattr(server, "convert_a2a_request_to_agent_run_request", fake_converter)
    monkeypatch.setattr(settings, "a2a_streaming", True)
    converted = server._bounded_request(  # noqa: SLF001 - request policy contract
        cast("RequestContext", None),
        cast("A2APartToGenAIPartConverter", None),
    )
    assert converted.run_config is not None
    assert converted.run_config.streaming_mode is server.StreamingMode.SSE
    assert converted.run_config.max_llm_calls == 12  # the budget survives the streaming override


def test_a2a_sse_delivers_incremental_events(monkeypatch) -> None:
    monkeypatch.setattr(settings, "a2a_streaming", True)
    events = _stream_events(fail_after_first=False)
    results = [event["result"] for event in events]

    artifacts = [result for result in results if result.get("kind") == "artifact-update"]
    assert len(artifacts) >= 2  # a final-only response would produce one artifact
    assert artifacts[0]["artifact"]["parts"][0]["text"] == "first "
    assert results[-1]["kind"] == "status-update"
    assert results[-1]["status"]["state"] == "completed"
    assert results[-1]["final"] is True


def test_a2a_sse_interruption_emits_terminal_failure(monkeypatch) -> None:
    monkeypatch.setattr(settings, "a2a_streaming", True)
    events = _stream_events(fail_after_first=True)
    terminal = events[-1]["result"]

    assert terminal["kind"] == "status-update"
    assert terminal["status"]["state"] == "failed", json.dumps(events, indent=2)
    assert terminal["final"] is True
    assert terminal["metadata"]["adk_error_code"] == "MODEL_UNAVAILABLE"
    assert terminal["status"]["message"]["parts"][0]["text"] == "Model request failed safely."
    assert "stream interrupted" not in json.dumps(events)
