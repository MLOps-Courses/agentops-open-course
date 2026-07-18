"""Offline contract tests for the tracked A2A server."""

import asyncio
import json
import sqlite3
from contextlib import closing
from pathlib import Path
from typing import Any, cast

from a2a.server.agent_execution import RequestContext
from google.adk.a2a.converters.part_converter import A2APartToGenAIPartConverter
from google.adk.a2a.converters.request_converter import AgentRunRequest
from google.adk.agents import Agent, RunConfig
from google.adk.models import BaseLlm, LlmRequest, LlmResponse
from google.genai import types
from sqlalchemy.ext.asyncio import create_async_engine
from starlette.testclient import TestClient

from agent import actions, data, server
from agent.config import settings
from agent.guardrails import handle_model_error, validate_actions


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


class _ConfirmationLlm(BaseLlm):
    """Deterministically request one guarded action, then summarize its result."""

    calls: int = 0

    async def generate_content_async(self, llm_request: LlmRequest, stream: bool = False):
        assert stream is False
        self.calls += 1
        if self.calls == 1:
            yield LlmResponse(
                content=types.Content(
                    role="model",
                    parts=[
                        types.Part(
                            function_call=types.FunctionCall(
                                id="restart-inventory-call",
                                name="restart_service",
                                args={"name": "inventory"},
                            )
                        )
                    ],
                )
            )
            return

        function_responses = [
            part.function_response
            for content in llm_request.contents
            for part in content.parts or []
            if part.function_response
        ]
        assert any(response.name == "restart_service" for response in function_responses)
        yield LlmResponse(
            content=types.Content(
                role="model",
                parts=[types.Part(text="The approved inventory restart completed and was audited.")],
            )
        )


def _stream_rpc_results(client: TestClient, payload: dict[str, Any]) -> list[dict[str, Any]]:
    with client.stream("POST", "/", json=payload) as response:
        assert response.status_code == 200
        assert response.headers["content-type"].startswith("text/event-stream")
        return [
            json.loads(line.removeprefix("data: "))["result"]
            for line in response.iter_lines()
            if line.startswith("data: ")
        ]


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
    with TestClient(server.create_app(agent)) as client:
        return [{"result": result} for result in _stream_rpc_results(client, payload)]


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
        assert runtime.session_service.db_engine is runtime.task_engine

        async with app.router.lifespan_context(app):
            await runtime.session_service.create_session(app_name="agentops-agent", user_id="test")
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
        "host": "127.0.0.1",
        "port": 8080,
        "timeout_graceful_shutdown": 10,  # bounded SIGTERM drain (Ch. 6)
    }


def test_container_explicitly_opts_into_an_a2a_network_bind() -> None:
    dockerfile = (Path(__file__).parents[1] / "Dockerfile").read_text(encoding="utf-8")
    assert "AGENT_A2A_BIND_HOST=0.0.0.0" in dockerfile


def test_a2a_confirmation_response_resumes_the_guarded_action_with_audit_identity() -> None:
    """Exercise the exact DataPart approval round-trip used by clients/web."""
    model = _ConfirmationLlm(model="confirmation-test")
    seen_contexts: list[tuple[str, str, str, bool]] = []

    def capture_tool_context(*, tool: Any, args: dict[str, Any], tool_context: Any) -> None:
        del tool, args
        seen_contexts.append(
            (
                tool_context.user_id,
                tool_context.session.id,
                tool_context.invocation_id,
                tool_context.tool_confirmation is not None,
            )
        )

    agent = Agent(
        name="confirmation_test_agent",
        instruction="Call restart_service for the requested inventory restart.",
        model=model,
        tools=[actions.ACTION_TOOLS[0]],
        before_tool_callback=[validate_actions, capture_tool_context],
    )
    app = server.create_app(agent)
    start_request = {
        "jsonrpc": "2.0",
        "id": "approval-start",
        "method": "message/stream",
        "params": {
            "message": {
                "kind": "message",
                "messageId": "approval-message",
                "role": "user",
                "parts": [{"kind": "text", "text": "Restart inventory."}],
            }
        },
    }

    with TestClient(app) as client:
        start_results = _stream_rpc_results(client, start_request)
        assert [result["kind"] for result in start_results] == [
            "task",
            "status-update",
            "artifact-update",
            "artifact-update",
            "status-update",
        ]
        assert start_results[0]["status"]["state"] == "submitted"
        assert start_results[1]["status"]["state"] == "working"
        paused_task = start_results[-1]
        assert paused_task["status"]["state"] == "input-required"
        assert paused_task["final"] is True
        context_id = paused_task["contextId"]
        task_id = paused_task["taskId"]
        confirmation_call = next(
            part["data"]
            for part in paused_task["status"]["message"]["parts"]
            if part["kind"] == "data"
            and part.get("metadata", {}).get("adk_type") == "function_call"
            and part["data"]["name"] == "adk_request_confirmation"
        )
        assert confirmation_call["args"]["originalFunctionCall"] == {
            "id": "restart-inventory-call",
            "args": {"name": "inventory"},
            "name": "restart_service",
        }
        rationale = "Inventory is down and INC-002 matches the service-down runbook."
        resume_request = {
            "jsonrpc": "2.0",
            "id": "approval-resume",
            "method": "message/stream",
            "params": {
                "message": {
                    "kind": "message",
                    "messageId": "approval-response",
                    "role": "user",
                    "contextId": context_id,
                    "taskId": task_id,
                    "parts": [
                        {
                            "kind": "data",
                            "data": {
                                "id": confirmation_call["id"],
                                "name": confirmation_call["name"],
                                "response": {
                                    "confirmed": True,
                                    "payload": {"rationale": rationale},
                                },
                            },
                            "metadata": {"adk_type": "function_response"},
                        }
                    ],
                }
            },
        }
        resume_results = _stream_rpc_results(client, resume_request)
        assert [result["kind"] for result in resume_results] == [
            "status-update",
            "artifact-update",
            "artifact-update",
            "status-update",
        ]
        completed_task = resume_results[-1]
        assert completed_task["taskId"] == task_id
        assert completed_task["contextId"] == context_id
        assert completed_task["status"]["state"] == "completed"
        assert completed_task["final"] is True

        service = data.get_service("inventory")
        assert service is not None
        assert service.status == "operational"
        with closing(sqlite3.connect(data.db_path())) as connection:
            connection.row_factory = sqlite3.Row
            audit = connection.execute(
                """
                SELECT approved_by, rationale, session_id, invocation_id, action, target
                FROM audit_log
                ORDER BY id DESC
                LIMIT 1
                """
            ).fetchone()
        assert audit is not None
        assert audit["approved_by"] == f"A2A_USER_{context_id}"
        assert audit["rationale"] == rationale
        assert audit["session_id"] == context_id
        assert seen_contexts == [
            (f"A2A_USER_{context_id}", context_id, audit["invocation_id"], False),
            (f"A2A_USER_{context_id}", context_id, audit["invocation_id"], True),
        ]
        assert audit["action"] == "restart_service"
        assert audit["target"] == "inventory"
        assert model.calls == 2


def test_health_endpoints_report_ready() -> None:
    runtime_database = settings.state_dir / "incidents.db"
    assert not runtime_database.exists()
    with TestClient(server.create_app()) as client:
        assert runtime_database.is_file()
        alive = client.get("/livez")
        assert alive.status_code == 200
        assert alive.json() == {"status": "alive"}
        ready = client.get("/healthz")
        assert ready.status_code == 200
        assert ready.json() == {"status": "ready"}
    assert runtime_database.is_file()


def test_readiness_fails_without_the_seed_dataset(tmp_path, monkeypatch) -> None:
    with TestClient(server.create_app()) as client:
        monkeypatch.setattr(settings, "data_dir", tmp_path / "missing")
        monkeypatch.setattr(settings, "state_dir", tmp_path / "fresh-state")
        response = client.get("/healthz")
        assert response.status_code == 503
        assert any("dataset unavailable" in problem for problem in response.json()["problems"])


def test_readiness_rejects_a_corrupt_runtime_database() -> None:
    settings.state_dir.mkdir(parents=True)
    destination = settings.state_dir / "incidents.db"
    destination.write_text("not a SQLite database", encoding="utf-8")
    before = destination.read_bytes()
    with TestClient(server.create_app()) as client:
        response = client.get("/healthz")
    assert response.status_code == 503
    assert any("dataset unavailable" in problem for problem in response.json()["problems"])
    assert destination.read_bytes() == before


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


def test_a2a_sse_crlf_frames_match_the_checked_in_browser_parser() -> None:
    agent = Agent(
        name="sse_wire_test_agent",
        instruction="Reply using the fake model.",
        model=_StreamingLlm(model="sse-wire-test"),
    )
    payload = {
        "jsonrpc": "2.0",
        "id": "sse-wire-test",
        "method": "message/stream",
        "params": {
            "message": {
                "kind": "message",
                "messageId": "sse-wire-message",
                "role": "user",
                "parts": [{"kind": "text", "text": "Stream a reply."}],
            }
        },
    }
    with TestClient(server.create_app(agent)) as client, client.stream("POST", "/", json=payload) as response:
        assert response.status_code == 200
        body = bytearray()
        for chunk in response.iter_raw():
            body.extend(chunk)
            if b"\r\n\r\n" in body:
                break
    assert b"\r\n\r\n" in body

    browser_source = (Path(__file__).parents[3] / "clients" / "web" / "index.html").read_text(encoding="utf-8")
    assert r"/\r\n\r\n|\n\n|\r\r/" in browser_source
    assert r"/\r\n|\r|\n/" in browser_source


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


def test_verified_identity_overrides_synthetic_user_when_present(monkeypatch) -> None:
    """A gateway-verified subject becomes the run's user_id (and thus approved_by)."""

    def fake_converter(request, part_converter) -> AgentRunRequest:
        del request, part_converter
        return AgentRunRequest(user_id="A2A_USER_ctx-1", run_config=RunConfig())

    monkeypatch.setattr(server, "convert_a2a_request_to_agent_run_request", fake_converter)
    token = server._VERIFIED_SUBJECT.set("alice@example.com")  # noqa: SLF001 - simulates the middleware
    try:
        converted = server._bounded_request(  # noqa: SLF001 - request policy contract
            cast("RequestContext", None),
            cast("A2APartToGenAIPartConverter", None),
        )
    finally:
        server._VERIFIED_SUBJECT.reset(token)  # noqa: SLF001
    assert converted.user_id == "alice@example.com"


def test_missing_verified_identity_keeps_the_synthetic_user(monkeypatch) -> None:
    def fake_converter(request, part_converter) -> AgentRunRequest:
        del request, part_converter
        return AgentRunRequest(user_id="A2A_USER_ctx-1", run_config=RunConfig())

    monkeypatch.setattr(server, "convert_a2a_request_to_agent_run_request", fake_converter)
    converted = server._bounded_request(  # noqa: SLF001 - request policy contract
        cast("RequestContext", None),
        cast("A2APartToGenAIPartConverter", None),
    )
    assert converted.user_id == "A2A_USER_ctx-1"  # unauthenticated fallback stands


def _run_middleware(header_name: str | None, headers: list[tuple[bytes, bytes]]) -> str | None:
    """Drive the ASGI middleware once and capture the subject downstream sees."""
    seen: dict[str, str | None] = {}

    async def downstream(scope, receive, send) -> None:
        del scope, receive, send
        seen["subject"] = server._VERIFIED_SUBJECT.get()  # noqa: SLF001

    async def _noop(*args: object, **kwargs: object) -> None:
        del args, kwargs

    middleware = server.VerifiedIdentityMiddleware(downstream)

    async def drive() -> None:
        await middleware({"type": "http", "headers": headers}, _noop, _noop)

    settings_header = settings.trusted_identity_header
    settings.trusted_identity_header = header_name
    try:
        asyncio.run(drive())
    finally:
        settings.trusted_identity_header = settings_header
    return seen["subject"]


def test_middleware_binds_the_trusted_header_when_configured() -> None:
    subject = _run_middleware("x-verified-subject", [(b"x-verified-subject", b"alice@example.com")])
    assert subject == "alice@example.com"
    # The binding is per-request: it does not leak once the middleware returns.
    assert server._VERIFIED_SUBJECT.get() is None  # noqa: SLF001


def test_middleware_ignores_the_header_when_not_configured() -> None:
    subject = _run_middleware(None, [(b"x-verified-subject", b"attacker@evil.example")])
    assert subject is None  # an un-configured header is never trusted
