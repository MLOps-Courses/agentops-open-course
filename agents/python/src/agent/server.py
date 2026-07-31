"""Persistent A2A server for the kagent BYO deployment contract."""

from __future__ import annotations

import asyncio
import contextvars
import os
import sqlite3
from collections.abc import AsyncGenerator, AsyncIterator, Awaitable, Callable
from contextlib import aclosing, asynccontextmanager, closing
from dataclasses import dataclass
from importlib.metadata import version
from pathlib import Path
from typing import Any, cast, override

import uvicorn
from a2a.server.agent_execution import RequestContext
from a2a.server.events import Event as A2AEvent
from a2a.server.events import EventQueue
from a2a.server.tasks import TaskStore, TaskUpdater
from a2a.server.tasks.database_task_store import DatabaseTaskStore
from a2a.types import AgentCapabilities, AgentCard, AgentInterface, AgentSkill, TaskStatusUpdateEvent
from google.adk.a2a.converters.part_converter import A2APartToGenAIPartConverter
from google.adk.a2a.converters.request_converter import AgentRunRequest, convert_a2a_request_to_agent_run_request
from google.adk.a2a.executor.a2a_agent_executor import A2aAgentExecutor, A2aAgentExecutorConfig
from google.adk.a2a.executor.config import ExecuteInterceptor
from google.adk.a2a.executor.executor_context import ExecutorContext
from google.adk.a2a.utils.agent_to_a2a import to_a2a
from google.adk.agents import BaseAgent, RunConfig
from google.adk.agents.run_config import StreamingMode
from google.adk.apps import App
from google.adk.errors.already_exists_error import AlreadyExistsError
from google.adk.events import Event
from google.adk.runners import Runner
from google.adk.sessions import DatabaseSessionService, Session
from google.genai import types
from sqlalchemy import text
from sqlalchemy.ext.asyncio import AsyncEngine
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import JSONResponse

from .composition import root_agent
from .config import settings
from .data import prepare_runtime_database, probe_runtime_database
from .governance import build_app
from .models import CURRENT_RUNTIME_SCHEMA_VERSION
from .state import recover_interrupted_restore

# The gateway-verified caller identity for the request currently being served.
# A pure-ASGI middleware sets it from the trusted header (same task as the
# executor, so the value is visible when the request converter runs); it is unset
# outside a request, where the synthetic A2A id remains the identity.
_VERIFIED_SUBJECT: contextvars.ContextVar[str | None] = contextvars.ContextVar("verified_subject", default=None)
_RUNTIME_REQUIRED_COLUMNS = {
    "adk_internal_metadata": frozenset({"key", "value"}),
    "app_states": frozenset({"app_name", "state", "update_time"}),
    "user_states": frozenset({"app_name", "user_id", "state", "update_time"}),
    "sessions": frozenset({"app_name", "user_id", "id", "state", "create_time", "update_time"}),
    "events": frozenset({"id", "app_name", "user_id", "session_id", "invocation_id", "timestamp", "event_data"}),
    "tasks": frozenset(
        {
            "id",
            "context_id",
            "kind",
            "owner",
            "last_updated",
            "status",
            "artifacts",
            "history",
            "protocol_version",
            "metadata",
        }
    ),
}


class _A2ASessionService(DatabaseSessionService):
    """Make the executor's empty-state get-or-create safe under first-use races."""

    @override
    async def create_session(
        self,
        *,
        app_name: str,
        user_id: str,
        state: dict[str, Any] | None = None,
        session_id: str | None = None,
    ) -> Session:
        try:
            return await super().create_session(
                app_name=app_name,
                user_id=user_id,
                state=state,
                session_id=session_id,
            )
        except AlreadyExistsError:
            # A2A's maintained executor performs get-then-create outside the
            # runner. Two first requests can both observe a miss. Recover only
            # its empty-state, explicit-id contract; never discard requested
            # state from a general create call.
            if session_id is None or state:
                raise
            existing = await self.get_session(
                app_name=app_name,
                user_id=user_id,
                session_id=session_id,
            )
            if existing is None:
                raise
            return existing


class _SessionSerializingRunner(Runner):
    """Run at most one ADK invocation per logical A2A session at a time.

    DatabaseSessionService returns a detached session snapshot. Locking only its
    final append cannot prevent two overlapping invocations from loading the
    same token total and losing one update, so acquire this process-local gate
    before ADK loads the session. Different sessions still run concurrently.
    """

    def __init__(
        self,
        *,
        app: App,
        session_service: DatabaseSessionService,
    ) -> None:
        super().__init__(app=app, session_service=session_service)
        self._session_locks_guard = asyncio.Lock()
        self._session_locks: dict[tuple[str, str], asyncio.Lock] = {}
        self._session_lock_refs: dict[tuple[str, str], int] = {}

    @asynccontextmanager
    async def _session_invocation(self, user_id: str, session_id: str) -> AsyncIterator[None]:
        """Hold one reference-counted lock for a complete session invocation."""
        key = user_id, session_id
        async with self._session_locks_guard:
            lock = self._session_locks.get(key)
            if lock is None:
                lock = asyncio.Lock()
                self._session_locks[key] = lock
            self._session_lock_refs[key] = self._session_lock_refs.get(key, 0) + 1

        try:
            async with lock:
                yield
        finally:
            async with self._session_locks_guard:
                remaining = self._session_lock_refs[key] - 1
                if remaining == 0:
                    self._session_lock_refs.pop(key)
                    self._session_locks.pop(key)
                else:
                    self._session_lock_refs[key] = remaining

    @override
    async def run_async(
        self,
        *,
        user_id: str,
        session_id: str,
        invocation_id: str | None = None,
        new_message: types.Content | None = None,
        state_delta: dict[str, Any] | None = None,
        run_config: RunConfig | None = None,
        yield_user_message: bool = False,
    ) -> AsyncGenerator[Event]:
        """Run one invocation after every earlier turn in its session finishes."""
        async with (
            self._session_invocation(user_id, session_id),
            aclosing(
                super().run_async(
                    user_id=user_id,
                    session_id=session_id,
                    invocation_id=invocation_id,
                    new_message=new_message,
                    state_delta=state_delta,
                    run_config=run_config,
                    yield_user_message=yield_user_message,
                )
            ) as events,
        ):
            async for event in events:
                yield event


# --8<-- [start:a2a-cancel]
class _CancelableA2AExecutor(A2aAgentExecutor):
    """Add the terminal cancellation event omitted by ADK's executor."""

    @override
    async def cancel(self, context: RequestContext, event_queue: EventQueue) -> None:
        if not context.task_id or not context.context_id:
            raise ValueError("A2A cancellation requires task and context ids.")
        # The A2A handler cancels the producer before calling this method. ADK
        # 2.4 leaves cancel() unimplemented, so publish the terminal protocol
        # event here instead of turning a user cancellation into a failed task.
        await TaskUpdater(event_queue, context.task_id, context.context_id).cancel()


# --8<-- [end:a2a-cancel]
class VerifiedIdentityMiddleware:
    """Bind the gateway-verified caller identity for the duration of one request.

    When ``AGENT_TRUSTED_IDENTITY_HEADER`` is set, this reads that header — which a
    trusted gateway populates *after* validating the JWT (Chapter 5.5) — and makes
    it the request's caller identity, so a guarded write's audit row names the
    real approver instead of the unauthenticated synthetic id. When unset, or when
    the header is absent, nothing changes and the synthetic id stands.
    """

    def __init__(self, app: Callable[..., Awaitable[None]]) -> None:
        self.app = app

    async def __call__(self, scope: dict, receive: Callable, send: Callable) -> None:
        header_name = settings.trusted_identity_header
        if scope["type"] != "http" or not header_name:
            await self.app(scope, receive, send)
            return
        wanted = header_name.lower().encode()
        subject = next(
            (value.decode().strip() for key, value in scope.get("headers", []) if key.lower() == wanted and value),
            None,
        )
        token = _VERIFIED_SUBJECT.set(subject or None)
        try:
            await self.app(scope, receive, send)
        finally:
            _VERIFIED_SUBJECT.reset(token)


@dataclass(slots=True)
class Runtime:
    """Resources owned by one ASGI application instance."""

    runner: Runner
    session_service: DatabaseSessionService
    task_engine: AsyncEngine
    task_store: DatabaseTaskStore

    async def close(self) -> None:
        """Close every resource even when an earlier cleanup step fails."""
        try:
            await self.runner.close()
        finally:
            await self.session_service.close()


agent_card = AgentCard(
    name="AgentOps Agent",
    description="Runbook-grounded incident triage and guarded remediation for the AgentOps Open Course.",
    # a2a-sdk 1.x replaced the single top-level ``url`` with a list of transport
    # interfaces; the JSON-RPC binding is the one ADK's ``to_a2a`` serves.
    supported_interfaces=[
        AgentInterface(
            url=f"{settings.a2a_protocol}://{settings.a2a_host}:{settings.a2a_port}/",
            protocol_binding="JSONRPC",
        )
    ],
    version=version("agentops-agent"),
    capabilities=AgentCapabilities(streaming=True),
    default_input_modes=["text/plain"],
    default_output_modes=["text/plain"],
    skills=[
        AgentSkill(
            id="incident-triage",
            name="Incident triage",
            description="Prioritize incidents using service state, logs, and deterministic severity rules.",
            tags=["incident", "triage", "operations"],
            examples=["Triage the open incidents."],
        ),
        AgentSkill(
            id="remediation",
            name="Guarded remediation",
            description="Recommend runbook-backed remediation and request confirmation before mock actions.",
            tags=["runbook", "remediation", "approval"],
            examples=["How should I remediate INC-002?"],
        ),
    ],
)


def _bounded_request(
    request: RequestContext,
    part_converter: A2APartToGenAIPartConverter,
) -> AgentRunRequest:
    """Convert an A2A request with a bounded model-call budget.

    ADK's converter never sets ``streaming_mode``, so A2A ``message/stream``
    clients get SSE of whole events while the model runs non-streaming. With
    ``AGENT_A2A_STREAMING=true``, model tokens stream as partial events too —
    an explicit trade-off documented in Chapter 3.6.
    """
    converted = convert_a2a_request_to_agent_run_request(request, part_converter)
    # --8<-- [start:verified-identity]
    # A gateway-verified identity, if present, replaces the synthetic A2A id so the
    # audit row (Chapter 7.6) and per-user memory key on the real caller.
    verified_subject = _VERIFIED_SUBJECT.get()
    if verified_subject:
        converted.user_id = verified_subject
    # --8<-- [end:verified-identity]
    run_config = converted.run_config or RunConfig()
    updates: dict[str, object] = {"max_llm_calls": settings.a2a_max_llm_calls}
    if settings.a2a_streaming:
        updates["streaming_mode"] = StreamingMode.SSE
    converted.run_config = run_config.model_copy(update=updates)
    return converted


def _agent_executor(runner: Runner) -> A2aAgentExecutor:
    """Create the maintained A2A executor with the bounded request policy."""
    return _CancelableA2AExecutor(
        runner=runner,
        config=A2aAgentExecutorConfig(
            request_converter=_bounded_request,
            execute_interceptors=[_error_code_interceptor()],
        ),
        # ADK's legacy result aggregator mutates terminal failures back to
        # ``working`` before enqueueing them. The maintained executor preserves
        # the A2A terminal state and emits partial model output as artifacts.
        force_new_version=True,
    )


def _error_code_interceptor() -> ExecuteInterceptor:
    """Carry ADK's structured error code onto the final A2A event.

    The maintained executor replaces error-event metadata with invocation
    metadata immediately before enqueueing the terminal update. Context-local
    state keeps concurrent streams isolated without leaving task-keyed residue
    when cancellation bypasses ``after_agent``.
    """
    error_code_context: contextvars.ContextVar[str | None] = contextvars.ContextVar(
        "a2a_error_code",
        default=None,
    )

    async def clear(request: RequestContext) -> RequestContext:
        error_code_context.set(None)
        return request

    async def remember(
        executor_context: ExecutorContext,
        a2a_event: A2AEvent,
        adk_event: Event,
    ) -> A2AEvent:
        del executor_context
        if adk_event.error_code and getattr(a2a_event, "task_id", None):
            error_code_context.set(str(adk_event.error_code))
        return a2a_event

    async def restore(
        executor_context: ExecutorContext,
        final_event: TaskStatusUpdateEvent,
    ) -> TaskStatusUpdateEvent:
        del executor_context
        error_code = error_code_context.get()
        error_code_context.set(None)
        if error_code:
            # a2a-sdk 1.x models ``metadata`` as a protobuf Struct: mutate the
            # entry in place rather than reassigning the whole submessage.
            final_event.metadata["adk_error_code"] = error_code
        return final_event

    return ExecuteInterceptor(before_agent=clear, after_event=remember, after_agent=restore)


async def _probe_runtime_store(task_engine: AsyncEngine) -> None:
    """Read and validate the bounded ADK session and A2A task schema."""
    async with task_engine.connect() as connection:
        integrity = (await connection.execute(text("PRAGMA quick_check(1)"))).scalar_one_or_none()
        if integrity != "ok":
            raise RuntimeError(f"runtime.db integrity check failed: {integrity or 'no result'}")
        tables = set(
            (
                await connection.execute(
                    text(
                        """
                        SELECT name
                        FROM sqlite_schema
                        WHERE type = 'table'
                        """
                    )
                )
            ).scalars()
        )
        missing_tables = _RUNTIME_REQUIRED_COLUMNS.keys() - tables
        if missing_tables:
            raise RuntimeError(f"runtime.db is missing tables: {', '.join(sorted(missing_tables))}")
        schema_version = (
            await connection.execute(
                text(
                    """
                    SELECT value
                    FROM adk_internal_metadata
                    WHERE "key" = 'schema_version'
                    """
                )
            )
        ).scalar_one_or_none()
        if schema_version != CURRENT_RUNTIME_SCHEMA_VERSION:
            raise RuntimeError(
                f"runtime.db has ADK schema {schema_version!r}; expected {CURRENT_RUNTIME_SCHEMA_VERSION!r}"
            )
        for table, required_columns in _RUNTIME_REQUIRED_COLUMNS.items():
            columns = set(
                (
                    await connection.execute(
                        text("SELECT name FROM pragma_table_info(:table)"),
                        {"table": table},
                    )
                ).scalars()
            )
            missing_columns = required_columns - columns
            if missing_columns:
                raise RuntimeError(
                    f"runtime.db table {table!r} is missing columns: {', '.join(sorted(missing_columns))}"
                )


def _preflight_existing_runtime_store(path: Path) -> None:
    """Reject incompatible existing session/task state before upstream writes."""
    if not path.exists():
        return
    try:
        with closing(sqlite3.connect(f"{path.resolve().as_uri()}?mode=ro", uri=True, timeout=5)) as connection:
            connection.execute("PRAGMA query_only = ON")
            integrity = connection.execute("PRAGMA quick_check(1)").fetchone()
            if integrity is None or integrity[0] != "ok":
                status = "no result" if integrity is None else str(integrity[0])
                raise RuntimeError(f"runtime.db integrity check failed before startup: {status}")
            tables = {
                row[0]
                for row in connection.execute(
                    """
                    SELECT name
                    FROM sqlite_schema
                    WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
                    """
                )
            }
            if not tables:
                return
            metadata_columns = {row[1] for row in connection.execute("PRAGMA table_info('adk_internal_metadata')")}
            if {"key", "value"} <= metadata_columns:
                row = connection.execute(
                    """
                    SELECT value
                    FROM adk_internal_metadata
                    WHERE "key" = 'schema_version'
                    """
                ).fetchone()
                schema_version = row[0] if row is not None else None
                if schema_version != CURRENT_RUNTIME_SCHEMA_VERSION:
                    raise RuntimeError(
                        f"runtime.db has ADK schema {schema_version!r}, but this application supports "
                        f"{CURRENT_RUNTIME_SCHEMA_VERSION!r}. Upgrade the application or select a compatible snapshot."
                    )
                missing_tables = set(_RUNTIME_REQUIRED_COLUMNS) - tables
                if missing_tables:
                    raise RuntimeError(
                        "runtime.db has an incomplete current schema and is missing tables: "
                        f"{', '.join(sorted(missing_tables))}. Select a compatible snapshot before startup."
                    )
                for table, required_columns in _RUNTIME_REQUIRED_COLUMNS.items():
                    columns = {
                        row[0]
                        for row in connection.execute(
                            "SELECT name FROM pragma_table_info(?)",
                            (table,),
                        )
                    }
                    missing_columns = required_columns - columns
                    if missing_columns:
                        raise RuntimeError(
                            f"runtime.db has an incomplete current schema: table {table!r} is missing columns "
                            f"{', '.join(sorted(missing_columns))}. Select a compatible snapshot before startup."
                        )
                return
            raise RuntimeError(
                "runtime.db has an unsupported legacy or malformed schema. "
                "Upgrade the application or select a compatible snapshot."
            )
    except sqlite3.Error as error:
        raise RuntimeError("runtime.db could not be inspected read-only before startup") from error


def _health_routes(runtime: Runtime) -> tuple[Callable[[Request], Awaitable[JSONResponse]], ...]:
    """Build the readiness and liveness handlers over one runtime's resources."""

    async def healthz(request: Request) -> JSONResponse:
        """Readiness: the process can actually serve, not merely open a port."""
        del request
        problems: list[str] = []
        try:
            probe_runtime_database()
        except Exception as error:  # readiness reports every failure class as unready
            problems.append(f"dataset unavailable: {type(error).__name__}")
        if not os.access(settings.state_dir, os.W_OK):
            problems.append(f"state directory is not writable: {settings.state_dir}")
        try:
            async with asyncio.timeout(5):
                await _probe_runtime_store(runtime.task_engine)
        except Exception as error:  # readiness reports invalid, corrupt, slow, and unreachable state
            problems.append(f"session/task store invalid: {type(error).__name__}")
        if problems:
            return JSONResponse({"status": "unready", "problems": problems}, status_code=503)
        return JSONResponse({"status": "ready"})

    async def livez(request: Request) -> JSONResponse:
        """Liveness: trivial by design — restarts only help a wedged process."""
        del request
        return JSONResponse({"status": "alive"})

    return healthz, livez


def _selected_a2a_agent() -> BaseAgent:
    """Require the conversational composition for the A2A server."""
    if isinstance(root_agent, BaseAgent):
        return root_agent
    raise RuntimeError("The A2A server requires AGENT_ENTRYPOINT=agent.")


def create_app(agent: BaseAgent | None = None) -> Starlette:
    """Build an A2A app whose SQLite resources have an explicit owner."""
    selected_agent = agent if agent is not None else _selected_a2a_agent()
    settings.state_dir.mkdir(parents=True, exist_ok=True)
    database_url = f"sqlite+aiosqlite:///{settings.state_dir / 'runtime.db'}"

    # Sessions and tasks share one connection pool because SQLite has one writer.
    # A single pooled connection queues their short transactions instead of
    # letting independent engines race into intermittent "database is locked".
    # --8<-- [start:a2a-runtime]
    session_service = _A2ASessionService(
        db_url=database_url,
        pool_size=1,
        max_overflow=0,
        connect_args={"timeout": 30},
    )
    # Build the App here rather than importing the module-level one, so an injected agent
    # (tests, embedding) is governed by the same policy plugin as the shipped composition.
    # ``app=`` is ADK's recommended Runner form; the deprecated ``agent=``/``plugins=`` pair
    # would attach the policy to only this Runner instead of to the application.
    runner = _SessionSerializingRunner(
        app=build_app(selected_agent),
        session_service=session_service,
    )
    task_engine = session_service.db_engine
    runtime = Runtime(
        runner=runner,
        session_service=session_service,
        task_engine=task_engine,
        task_store=DatabaseTaskStore(engine=task_engine),
    )
    # --8<-- [end:a2a-runtime]

    # --8<-- [start:a2a-lifespan]
    @asynccontextmanager
    async def lifespan(_: Starlette):
        try:
            # The writable A2A process owns first-boot publication. Readiness
            # remains a strictly read-only observation after startup. Both
            # upstream stores create their schemas lazily, so initialize them
            # here before readiness is allowed to report success.
            recover_interrupted_restore(settings.state_dir)
            _preflight_existing_runtime_store(settings.state_dir / "runtime.db")
            prepare_runtime_database()
            await runtime.session_service.prepare_tables()
            await runtime.task_store.initialize()
            yield
        finally:
            await runtime.close()

    # --8<-- [end:a2a-lifespan]
    app = to_a2a(
        selected_agent,
        host=settings.a2a_host,
        port=settings.a2a_port,
        protocol=settings.a2a_protocol,
        agent_card=agent_card,
        runner=runtime.runner,
        # a2a-sdk exposes duplicate aliases in its type surface, while this is
        # the concrete TaskStore implementation accepted at runtime.
        task_store=cast("TaskStore", runtime.task_store),
        # ADK documents an async context manager here but annotates an iterator.
        lifespan=cast("Callable[[Starlette], AsyncIterator[None]]", lifespan),
        agent_executor_factory=_agent_executor,
    )
    app.state.runtime = runtime
    # Bind the gateway-verified identity (when configured) for each request before
    # the A2A executor converts it, so a guarded write audits the real approver.
    app.add_middleware(VerifiedIdentityMiddleware)
    # Kubernetes-facing health endpoints (Ch. 6): registered before startup so
    # they coexist with the A2A routes the lifespan adds.
    healthz, livez = _health_routes(runtime)
    app.add_route("/healthz", healthz, methods=["GET"])
    app.add_route("/livez", livez, methods=["GET"])
    return app


def main() -> None:
    """Serve A2A on the configured listener while advertising its callable host.

    Uvicorn owns SIGTERM: it stops accepting connections and gives in-flight
    requests up to ``AGENT_DRAIN_TIMEOUT_S`` to finish before forcing shutdown.
    The bound reduces avoidable interruption; a turn that exceeds it can still
    be cut, so state changes remain transactional rather than relying on drain
    time for correctness.
    """
    uvicorn.run(
        create_app,
        factory=True,
        host=settings.a2a_bind_host,
        port=settings.a2a_port,
        timeout_graceful_shutdown=int(settings.drain_timeout_s),
    )


if __name__ == "__main__":
    main()
