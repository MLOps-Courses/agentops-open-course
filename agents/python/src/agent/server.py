"""Persistent A2A server for the kagent BYO deployment contract."""

from __future__ import annotations

import contextvars
import os
from collections.abc import AsyncIterator, Awaitable, Callable
from contextlib import asynccontextmanager
from dataclasses import dataclass
from importlib.metadata import version
from typing import cast

import uvicorn
from a2a.server.agent_execution import RequestContext
from a2a.server.events import Event as A2AEvent
from a2a.server.tasks import DatabaseTaskStore, TaskStore
from a2a.types import AgentCapabilities, AgentCard, AgentSkill, TaskStatusUpdateEvent
from google.adk.a2a.converters.part_converter import A2APartToGenAIPartConverter
from google.adk.a2a.converters.request_converter import AgentRunRequest, convert_a2a_request_to_agent_run_request
from google.adk.a2a.executor.a2a_agent_executor import A2aAgentExecutor, A2aAgentExecutorConfig
from google.adk.a2a.executor.config import ExecuteInterceptor
from google.adk.a2a.executor.executor_context import ExecutorContext
from google.adk.a2a.utils.agent_to_a2a import to_a2a
from google.adk.agents import BaseAgent, RunConfig
from google.adk.agents.run_config import StreamingMode
from google.adk.events import Event
from google.adk.runners import Runner
from google.adk.sessions import DatabaseSessionService
from sqlalchemy import text
from sqlalchemy.ext.asyncio import AsyncEngine
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import JSONResponse

from .agent import root_agent
from .config import settings
from .data import db_path, probe_runtime_database

_APP_NAME = "agentops-agent"

# The gateway-verified caller identity for the request currently being served.
# A pure-ASGI middleware sets it from the trusted header (same task as the
# executor, so the value is visible when the request converter runs); it is unset
# outside a request, where the synthetic A2A id remains the identity.
_VERIFIED_SUBJECT: contextvars.ContextVar[str | None] = contextvars.ContextVar("verified_subject", default=None)


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
    url=f"{settings.a2a_protocol}://{settings.a2a_host}:{settings.a2a_port}/",
    version=version("agentops-agent"),
    capabilities=AgentCapabilities(streaming=True, state_transition_history=True),
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
    return A2aAgentExecutor(
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
    metadata immediately before enqueueing the terminal update. Task-keyed
    state keeps concurrent streams isolated while retaining both contracts.
    """
    error_codes: dict[str, str] = {}

    async def remember(
        executor_context: ExecutorContext,
        a2a_event: A2AEvent,
        adk_event: Event,
    ) -> A2AEvent:
        del executor_context
        task_id = getattr(a2a_event, "task_id", None)
        if adk_event.error_code and task_id:
            error_codes[task_id] = str(adk_event.error_code)
        return a2a_event

    async def restore(
        executor_context: ExecutorContext,
        final_event: TaskStatusUpdateEvent,
    ) -> TaskStatusUpdateEvent:
        del executor_context
        error_code = error_codes.pop(final_event.task_id, None)
        if error_code:
            final_event.metadata = {**(final_event.metadata or {}), "adk_error_code": error_code}
        return final_event

    return ExecuteInterceptor(after_event=remember, after_agent=restore)


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
            async with runtime.task_engine.connect() as connection:
                await connection.execute(text("SELECT 1"))
        except Exception as error:  # session store: corrupt/unreachable SQLite
            problems.append(f"session store unreachable: {type(error).__name__}")
        if problems:
            return JSONResponse({"status": "unready", "problems": problems}, status_code=503)
        return JSONResponse({"status": "ready"})

    async def livez(request: Request) -> JSONResponse:
        """Liveness: trivial by design — restarts only help a wedged process."""
        del request
        return JSONResponse({"status": "alive"})

    return healthz, livez


def create_app(agent: BaseAgent = root_agent) -> Starlette:
    """Build an A2A app whose SQLite resources have an explicit owner."""
    settings.state_dir.mkdir(parents=True, exist_ok=True)
    database_url = f"sqlite+aiosqlite:///{settings.state_dir / 'runtime.db'}"

    # Sessions and tasks share one connection pool because SQLite has one writer.
    # A single pooled connection queues their short transactions instead of
    # letting independent engines race into intermittent "database is locked".
    session_service = DatabaseSessionService(
        db_url=database_url,
        pool_size=1,
        max_overflow=0,
        connect_args={"timeout": 30},
    )
    runner = Runner(agent=agent, app_name=_APP_NAME, session_service=session_service)
    task_engine = session_service.db_engine
    runtime = Runtime(
        runner=runner,
        session_service=session_service,
        task_engine=task_engine,
        task_store=DatabaseTaskStore(engine=task_engine),
    )

    @asynccontextmanager
    async def lifespan(_: Starlette):
        try:
            # The writable A2A process owns first-boot publication. Readiness
            # remains a strictly read-only observation after startup.
            db_path()
            yield
        finally:
            await runtime.close()

    app = to_a2a(
        agent,
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
