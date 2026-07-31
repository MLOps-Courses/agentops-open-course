"""Offline contract tests for the tracked A2A server."""

import asyncio
import json
import sqlite3
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from contextlib import closing
from importlib.metadata import version
from pathlib import Path
from types import SimpleNamespace
from typing import Any, cast

import pytest
from a2a.server.agent_execution import RequestContext
from google.adk.a2a.converters.part_converter import A2APartToGenAIPartConverter
from google.adk.a2a.converters.request_converter import AgentRunRequest
from google.adk.agents import Agent, RunConfig
from google.adk.apps import App
from google.adk.models import BaseLlm, LlmRequest, LlmResponse
from google.genai import types
from sqlalchemy.ext.asyncio import AsyncEngine
from starlette.testclient import TestClient

from agent import actions, data, server
from agent.config import settings
from agent.guardrails import handle_model_error, validate_actions
from agent.models import CURRENT_AUDIT_SCHEMA_VERSION, CURRENT_RUNTIME_SCHEMA_VERSION


def test_a2a_default_rejects_a_non_agent_entrypoint(monkeypatch) -> None:
    monkeypatch.setattr(server, "root_agent", object())
    with pytest.raises(RuntimeError, match="AGENT_ENTRYPOINT=agent"):
        server._selected_a2a_agent()  # noqa: SLF001 - entrypoint boundary


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


class _BlockingLlm(BaseLlm):
    """Hold one model call until the A2A task is canceled."""

    started: threading.Event

    async def generate_content_async(self, llm_request: LlmRequest, stream: bool = False):
        del llm_request, stream
        self.started.set()
        await asyncio.sleep(5)
        yield LlmResponse(content=types.Content(role="model", parts=[types.Part(text="too late")]))


class _UnreachableEngine:
    """Fail before opening a worker thread; readiness only needs the boundary."""

    def connect(self):
        raise OSError("session store unavailable")


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
    # a2a-sdk 1.x moved the endpoint from a single ``url`` to a transport-interface list.
    assert [interface.url for interface in server.agent_card.supported_interfaces] == ["http://localhost:8080/"]
    # Derive, never hard-code: the card must carry the installed distribution version, and a release
    # bump should not turn this suite red. `check:release-metadata` is what pins the number itself.
    assert server.agent_card.version == version("agentops-agent")
    assert "Operating rules" not in server.agent_card.description
    assert {skill.id for skill in server.agent_card.skills} == {"incident-triage", "remediation"}


def test_app_factory_owns_closes_and_prepares_persistent_runtime(monkeypatch) -> None:
    original = server.prepare_runtime_database
    original_recover = server.recover_interrupted_restore
    prepared = False
    startup_order: list[str] = []

    def tracked_recover(path: Path) -> None:
        startup_order.append("recover")
        original_recover(path)

    def tracked_prepare() -> Path:
        nonlocal prepared
        startup_order.append("prepare")
        prepared = True
        return original()

    monkeypatch.setattr(server, "recover_interrupted_restore", tracked_recover)
    monkeypatch.setattr(server, "prepare_runtime_database", tracked_prepare)

    async def exercise_lifespan() -> None:
        app = server.create_app()
        runtime = app.state.runtime
        assert runtime.runner.session_service is runtime.session_service
        assert isinstance(runtime.session_service, server.DatabaseSessionService)
        assert runtime.session_service.__class__.__name__ == "_A2ASessionService"
        assert runtime.task_store.__class__.__name__ == "DatabaseTaskStore"
        assert runtime.session_service.db_engine is runtime.task_engine

        async with app.router.lifespan_context(app):
            assert prepared is True
            assert startup_order == ["recover", "prepare"]
            await runtime.session_service.create_session(app_name="agentops-agent", user_id="test")
            await runtime.task_store.initialize()

    asyncio.run(exercise_lifespan())


def test_a2a_session_service_recovers_a_concurrent_first_use(tmp_path: Path) -> None:
    async def exercise() -> None:
        service = server._A2ASessionService(  # noqa: SLF001 - A2A race boundary under test
            db_url=f"sqlite+aiosqlite:///{tmp_path / 'sessions.db'}",
            pool_size=1,
            max_overflow=0,
        )
        checked = 0
        both_checked = asyncio.Event()

        async def resolve() -> server.Session:
            nonlocal checked
            existing = await service.get_session(
                app_name="agentops-agent",
                user_id="test-user",
                session_id="shared-session",
            )
            assert existing is None
            checked += 1
            if checked == 2:
                both_checked.set()
            await both_checked.wait()
            return await service.create_session(
                app_name="agentops-agent",
                user_id="test-user",
                state={},
                session_id="shared-session",
            )

        try:
            first, second = await asyncio.gather(resolve(), resolve())
            assert first.id == second.id == "shared-session"
            listed = await service.list_sessions(app_name="agentops-agent", user_id="test-user")
            assert [session.id for session in listed.sessions] == ["shared-session"]
        finally:
            await service.close()

    asyncio.run(exercise())


def test_a2a_runner_serializes_detached_snapshots_for_one_session(monkeypatch) -> None:
    """A second turn must load state only after the first one persists it."""

    async def exercise() -> None:
        persisted: dict[tuple[str, str], int] = {}
        snapshots: list[int] = []
        first_snapshot_loaded = asyncio.Event()
        release_first = asyncio.Event()

        async def detached_snapshot_run(
            runner,
            *,
            user_id: str,
            session_id: str,
            invocation_id: str | None = None,
            new_message: types.Content | None = None,
            state_delta: dict[str, Any] | None = None,
            run_config: RunConfig | None = None,
            yield_user_message: bool = False,
        ):
            del runner, new_message, state_delta, run_config, yield_user_message
            key = user_id, session_id
            snapshot = persisted.get(key, 0)
            snapshots.append(snapshot)
            if len(snapshots) == 1:
                first_snapshot_loaded.set()
                await release_first.wait()
            persisted[key] = snapshot + 1
            yield server.Event(author="test", invocation_id=invocation_id or "test")

        monkeypatch.setattr(server.Runner, "run_async", detached_snapshot_run)
        runner = server._SessionSerializingRunner(  # noqa: SLF001 - concurrency boundary under test
            app=App(
                name="agentops-agent",
                root_agent=Agent(
                    name="serialized_test_agent",
                    instruction="Reply using the fake model.",
                    model=_StreamingLlm(model="serialized-test"),
                ),
            ),
            session_service=cast("Any", object()),
        )

        async def run(invocation_id: str) -> list[server.Event]:
            return [
                event
                async for event in runner.run_async(
                    user_id="test-user",
                    session_id="test-session",
                    invocation_id=invocation_id,
                )
            ]

        first = asyncio.create_task(run("first"))
        await first_snapshot_loaded.wait()
        second = asyncio.create_task(run("second"))
        await asyncio.sleep(0)
        release_first.set()
        await asyncio.gather(first, second)

        assert snapshots == [0, 1]
        assert persisted[("test-user", "test-session")] == 2
        assert not runner._session_locks  # noqa: SLF001 - reference cleanup contract
        assert not runner._session_lock_refs  # noqa: SLF001 - reference cleanup contract

    asyncio.run(exercise())


def test_a2a_runner_keeps_different_sessions_concurrent(monkeypatch) -> None:
    async def exercise() -> None:
        both_started = asyncio.Event()
        active = 0
        peak_active = 0

        async def concurrent_run(runner, **kwargs):
            nonlocal active, peak_active
            del runner, kwargs
            active += 1
            peak_active = max(peak_active, active)
            if active == 2:
                both_started.set()
            await both_started.wait()
            active -= 1
            yield server.Event(author="test")

        monkeypatch.setattr(server.Runner, "run_async", concurrent_run)
        runner = server._SessionSerializingRunner(  # noqa: SLF001 - concurrency boundary under test
            app=App(
                name="agentops-agent",
                root_agent=Agent(
                    name="parallel_test_agent",
                    instruction="Reply using the fake model.",
                    model=_StreamingLlm(model="parallel-test"),
                ),
            ),
            session_service=cast("Any", object()),
        )

        async def run(session_id: str) -> None:
            async for _ in runner.run_async(user_id="test-user", session_id=session_id):
                pass

        await asyncio.wait_for(
            asyncio.gather(run("first-session"), run("second-session")),
            timeout=1,
        )
        assert peak_active == 2

    asyncio.run(exercise())


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
            "status-update",
            "artifact-update",
            "status-update",
        ]
        assert start_results[0]["status"]["state"] == "submitted"
        assert start_results[1]["status"]["state"] == "working"
        paused_task = start_results[-1]
        assert paused_task["status"]["state"] == "input-required"
        # a2a-sdk 1.x treats an input-required pause as non-terminal (the task resumes under
        # the same id), so ``final`` is False here even though the stream closes.
        assert paused_task["final"] is False
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


def test_a2a_task_cancellation_stops_the_turn_and_persists_terminal_state() -> None:
    started = threading.Event()
    agent = Agent(
        name="cancellation_test_agent",
        instruction="Reply using the fake model.",
        model=_BlockingLlm(model="cancellation-test", started=started),
    )
    start_request = {
        "jsonrpc": "2.0",
        "id": "cancel-start",
        "method": "message/stream",
        "params": {
            "message": {
                "kind": "message",
                "messageId": "cancel-message",
                "role": "user",
                "parts": [{"kind": "text", "text": "Wait for cancellation."}],
            }
        },
    }

    with TestClient(server.create_app(agent)) as client:
        with ThreadPoolExecutor(max_workers=1) as pool:
            stream = pool.submit(_stream_rpc_results, client, start_request)
            assert started.wait(timeout=2)
            deadline = time.monotonic() + 2
            task_id = ""
            while time.monotonic() < deadline:
                with closing(sqlite3.connect(settings.state_dir / "runtime.db")) as connection:
                    row = connection.execute("SELECT id FROM tasks ORDER BY rowid DESC LIMIT 1").fetchone()
                if row:
                    task_id = row[0]
                    break
                time.sleep(0.01)
            assert task_id
            cancel_response = client.post(
                "/",
                json={
                    "jsonrpc": "2.0",
                    "id": "cancel-task",
                    "method": "tasks/cancel",
                    "params": {"id": task_id},
                },
            )
            assert cancel_response.status_code == 200
            assert cancel_response.json()["result"]["status"]["state"] == "canceled"
            stream_results = stream.result(timeout=2)

        assert stream_results[-1]["status"]["state"] == "canceled"
        with closing(sqlite3.connect(settings.state_dir / "runtime.db")) as connection:
            status_row = connection.execute("SELECT status FROM tasks WHERE id = ?", (task_id,)).fetchone()
        assert status_row is not None
        assert json.loads(status_row[0])["state"] == "TASK_STATE_CANCELED"


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


def test_startup_rejects_a_corrupt_runtime_database_without_replacing_it() -> None:
    settings.state_dir.mkdir(parents=True)
    destination = settings.state_dir / "incidents.db"
    destination.write_text("not a SQLite database", encoding="utf-8")
    before = destination.read_bytes()
    with pytest.raises(data.DataAccessError, match="SQLite operation failed"), TestClient(server.create_app()):
        pass
    assert destination.read_bytes() == before


def test_readiness_fails_when_the_session_store_is_unreachable() -> None:
    app = server.create_app()
    with TestClient(app) as client:
        runtime = app.state.runtime
        healthy_engine = runtime.task_engine
        runtime.task_engine = cast("AsyncEngine", _UnreachableEngine())
        try:
            response = client.get("/healthz")
        finally:
            runtime.task_engine = healthy_engine
        assert response.status_code == 503
        assert any("session/task store invalid" in problem for problem in response.json()["problems"])


def test_readiness_rejects_a_connectable_runtime_database_with_missing_tables() -> None:
    with TestClient(server.create_app()) as client:
        with closing(sqlite3.connect(settings.state_dir / "runtime.db")) as connection:
            connection.execute("DROP TABLE tasks")
            connection.commit()

        response = client.get("/healthz")

    assert response.status_code == 503
    assert any("session/task store invalid" in problem for problem in response.json()["problems"])


def test_readiness_rejects_runtime_schema_corruption_introduced_after_startup() -> None:
    with TestClient(server.create_app()) as client:
        with closing(sqlite3.connect(settings.state_dir / "runtime.db")) as connection:
            connection.execute("UPDATE adk_internal_metadata SET value = '99' WHERE key = 'schema_version'")
            connection.commit()

        response = client.get("/healthz")

    assert response.status_code == 503
    assert any("session/task store invalid" in problem for problem in response.json()["problems"])


def test_startup_rejects_future_audit_state_without_mutating_it() -> None:
    path = data.db_path()
    with closing(sqlite3.connect(path)) as connection:
        connection.execute(
            """
            INSERT INTO audit_log
                (schema_version, ts, actor, approved_by, rationale, context_summary,
                 session_id, invocation_id, action, target, detail)
            VALUES (?, '2026-07-31T08:00:00Z', 'future-agent', 'engineer',
                    'future approval', 'future context', 'future-session',
                    'future-invocation', 'restart_service', 'inventory', 'future detail')
            """,
            (CURRENT_AUDIT_SCHEMA_VERSION + 1,),
        )
        connection.commit()
    before = path.read_bytes()

    with (
        pytest.raises(data.DataAccessError, match="Upgrade the application or select a compatible snapshot"),
        TestClient(server.create_app()),
    ):
        pass

    assert path.read_bytes() == before


def test_startup_rejects_future_runtime_state_without_mutating_it() -> None:
    settings.state_dir.mkdir(parents=True)
    path = settings.state_dir / "runtime.db"
    with closing(sqlite3.connect(path)) as connection:
        connection.execute(
            """
            CREATE TABLE adk_internal_metadata (
                "key" TEXT PRIMARY KEY,
                value TEXT NOT NULL
            )
            """
        )
        connection.execute("INSERT INTO adk_internal_metadata VALUES ('schema_version', '99')")
        connection.commit()
    before = path.read_bytes()
    incidents_path = settings.state_dir / "incidents.db"
    assert not incidents_path.exists()

    with (
        pytest.raises(RuntimeError, match="Upgrade the application or select a compatible snapshot"),
        TestClient(server.create_app()),
    ):
        pass

    assert path.read_bytes() == before
    assert not incidents_path.exists()


def test_startup_rejects_incomplete_current_runtime_state_without_mutating_it() -> None:
    settings.state_dir.mkdir(parents=True)
    path = settings.state_dir / "runtime.db"
    with closing(sqlite3.connect(path)) as connection:
        connection.execute(
            """
            CREATE TABLE adk_internal_metadata (
                "key" TEXT PRIMARY KEY,
                value TEXT NOT NULL
            )
            """
        )
        connection.execute(
            "INSERT INTO adk_internal_metadata VALUES ('schema_version', ?)",
            (CURRENT_RUNTIME_SCHEMA_VERSION,),
        )
        connection.commit()
    before = path.read_bytes()
    incidents_path = settings.state_dir / "incidents.db"
    assert not incidents_path.exists()

    with (
        pytest.raises(RuntimeError, match="incomplete current schema"),
        TestClient(server.create_app()),
    ):
        pass

    assert path.read_bytes() == before
    assert not incidents_path.exists()


def test_error_interceptor_cancellation_cannot_leak_state_into_a_later_task() -> None:
    async def exercise() -> None:
        interceptor = server._error_code_interceptor()  # noqa: SLF001 - cancellation cleanup boundary
        assert interceptor.before_agent is not None
        assert interceptor.after_event is not None
        assert interceptor.after_agent is not None
        request = cast("RequestContext", SimpleNamespace())
        context = cast("server.ExecutorContext", SimpleNamespace())

        await interceptor.before_agent(request)
        await interceptor.after_event(
            context,
            cast("server.A2AEvent", SimpleNamespace(task_id="canceled-task")),
            server.Event(author="test", error_code="MODEL_UNAVAILABLE"),
        )
        # Cancellation terminates the producer without calling after_agent.
        await interceptor.before_agent(request)
        later = cast(
            "server.TaskStatusUpdateEvent",
            SimpleNamespace(task_id="later-task", metadata={}),
        )
        restored = await interceptor.after_agent(context, later)

        assert "adk_error_code" not in restored.metadata

    asyncio.run(exercise())


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
    assert '"tasks/cancel"' in browser_source
    assert 'aria-label="Cancel the active task"' in browser_source


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
