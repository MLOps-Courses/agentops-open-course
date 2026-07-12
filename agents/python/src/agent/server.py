"""Persistent A2A server for the kagent BYO deployment contract."""

from __future__ import annotations

import os
from collections.abc import AsyncIterator, Awaitable, Callable
from contextlib import asynccontextmanager
from dataclasses import dataclass
from importlib.metadata import version
from typing import cast

import uvicorn
from a2a.server.agent_execution import RequestContext
from a2a.server.tasks import DatabaseTaskStore, TaskStore
from a2a.types import AgentCapabilities, AgentCard, AgentSkill
from google.adk.a2a.converters.part_converter import A2APartToGenAIPartConverter
from google.adk.a2a.converters.request_converter import AgentRunRequest, convert_a2a_request_to_agent_run_request
from google.adk.a2a.executor.a2a_agent_executor import A2aAgentExecutor, A2aAgentExecutorConfig
from google.adk.a2a.utils.agent_to_a2a import to_a2a
from google.adk.agents import RunConfig
from google.adk.agents.run_config import StreamingMode
from google.adk.runners import Runner
from google.adk.sessions import DatabaseSessionService
from sqlalchemy import text
from sqlalchemy.ext.asyncio import AsyncEngine, create_async_engine
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import JSONResponse

from .agent import root_agent
from .config import settings
from .data import db_path

_APP_NAME = "ops-copilot"


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
            try:
                await self.session_service.close()
            finally:
                await self.task_engine.dispose()


agent_card = AgentCard(
    name="Ops Copilot",
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
    run_config = converted.run_config or RunConfig()
    updates: dict[str, object] = {"max_llm_calls": settings.a2a_max_llm_calls}
    if settings.a2a_streaming:
        updates["streaming_mode"] = StreamingMode.SSE
    converted.run_config = run_config.model_copy(update=updates)
    return converted


def _agent_executor(runner: Runner) -> A2aAgentExecutor:
    """Create the A2A executor with the course's bounded request policy."""
    return A2aAgentExecutor(runner=runner, config=A2aAgentExecutorConfig(request_converter=_bounded_request))


def _health_routes(runtime: Runtime) -> tuple[Callable[[Request], Awaitable[JSONResponse]], ...]:
    """Build the readiness and liveness handlers over one runtime's resources."""

    async def healthz(request: Request) -> JSONResponse:
        """Readiness: the process can actually serve, not merely open a port."""
        del request
        problems: list[str] = []
        try:
            db_path()  # exercises the seed dataset and the writable state dir together
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


def create_app() -> Starlette:
    """Build an A2A app whose SQLite resources have an explicit owner."""
    settings.state_dir.mkdir(parents=True, exist_ok=True)
    database_url = f"sqlite+aiosqlite:///{settings.state_dir / 'runtime.db'}"

    session_service = DatabaseSessionService(db_url=database_url)
    runner = Runner(agent=root_agent, app_name=_APP_NAME, session_service=session_service)
    task_engine = create_async_engine(database_url)
    runtime = Runtime(
        runner=runner,
        session_service=session_service,
        task_engine=task_engine,
        task_store=DatabaseTaskStore(engine=task_engine),
    )

    @asynccontextmanager
    async def lifespan(_: Starlette):
        try:
            yield
        finally:
            await runtime.close()

    app = to_a2a(
        root_agent,
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
    # Kubernetes-facing health endpoints (Ch. 6): registered before startup so
    # they coexist with the A2A routes the lifespan adds.
    healthz, livez = _health_routes(runtime)
    app.add_route("/healthz", healthz, methods=["GET"])
    app.add_route("/livez", livez, methods=["GET"])
    return app


def main() -> None:
    """Serve A2A on all interfaces while advertising the configured public host.

    uvicorn owns SIGTERM: it stops accepting connections, drains in-flight
    requests for at most ``AGENT_DRAIN_TIMEOUT_S``, then exits cleanly — so pod
    rotation never cuts an agent turn mid-action.
    """
    uvicorn.run(
        create_app,
        factory=True,
        host="0.0.0.0",  # noqa: S104 - container bind
        port=settings.a2a_port,
        timeout_graceful_shutdown=int(settings.drain_timeout_s),
    )


if __name__ == "__main__":
    main()
