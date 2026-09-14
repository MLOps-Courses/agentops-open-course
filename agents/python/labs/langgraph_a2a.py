"""Optional LangGraph + official A2A SDK comparison; deterministic and read-only."""

from __future__ import annotations

import json
import re
from contextlib import asynccontextmanager

import uvicorn
from a2a.helpers import new_text_part
from a2a.server.agent_execution import AgentExecutor, RequestContext
from a2a.server.events import EventQueue
from a2a.server.request_handlers import DefaultRequestHandler
from a2a.server.routes.agent_card_routes import create_agent_card_routes
from a2a.server.routes.jsonrpc_routes import create_jsonrpc_routes
from a2a.server.tasks import InMemoryTaskStore, TaskUpdater
from a2a.types import AgentCapabilities, AgentCard, AgentInterface, AgentSkill, Task, TaskState, TaskStatus
from langgraph.graph import END, START, StateGraph
from pydantic import BaseModel
from starlette.applications import Starlette

from agent.tools import get_incident


class Investigation(BaseModel):
    """The graph's explicit input and evidence state."""

    incident_id: str
    evidence: str = ""
    answer: str = ""


def retrieve(state: Investigation) -> dict[str, str]:
    """Read one incident; reject malformed identifiers before reaching the tool."""
    if not re.fullmatch(r"INC-\d{3}", state.incident_id, flags=re.ASCII):
        return {"evidence": "Invalid incident ID; use INC-002."}
    return {"evidence": json.dumps(get_incident(state.incident_id), sort_keys=True)}


def recommend(state: Investigation) -> dict[str, str]:
    """Return evidence without invented remediation or an LLM call."""
    return {"answer": f"Read-only evidence: {state.evidence}"}


def build_graph():
    graph = StateGraph(Investigation)
    graph.add_node("retrieve", retrieve)
    graph.add_node("recommend", recommend)
    graph.add_edge(START, "retrieve")
    graph.add_edge("retrieve", "recommend")
    graph.add_edge("recommend", END)
    return graph.compile()


class InvestigationExecutor(AgentExecutor):
    """Adapt LangGraph to A2A without ADK, LangSmith, or an account."""

    async def execute(self, context: RequestContext, event_queue: EventQueue) -> None:
        result = await build_graph().ainvoke({"incident_id": context.get_user_input().strip()})
        if not context.task_id or not context.context_id:
            raise ValueError("A2A task and context identifiers are required")
        await event_queue.enqueue_event(
            Task(
                id=context.task_id,
                context_id=context.context_id,
                status=TaskStatus(state=TaskState.TASK_STATE_SUBMITTED),
            )
        )
        updater = TaskUpdater(event_queue, context.task_id, context.context_id)
        await updater.add_artifact([new_text_part(result["answer"])], name="evidence")
        await updater.complete()

    async def cancel(self, context: RequestContext, event_queue: EventQueue) -> None:
        del context, event_queue
        raise NotImplementedError("This stateless comparison does not expose cancellable tasks.")


def create_app() -> Starlette:
    card = AgentCard(
        name="LangGraph incident comparison",
        description="Deterministic read-only graph exposed through the official A2A SDK.",
        version="1.0.0",
        supported_interfaces=[AgentInterface(url="http://127.0.0.1:8080/", protocol_binding="JSONRPC")],
        capabilities=AgentCapabilities(streaming=False),
        default_input_modes=["text/plain"],
        default_output_modes=["text/plain"],
        skills=[
            AgentSkill(
                id="incident-evidence",
                name="Incident evidence",
                description="Read one fictional incident.",
                tags=["incident"],
            )
        ],
    )
    handler = DefaultRequestHandler(
        agent_card=card, agent_executor=InvestigationExecutor(), task_store=InMemoryTaskStore()
    )

    @asynccontextmanager
    async def lifespan(_app):
        try:
            yield
        finally:
            await handler.aclose()

    return Starlette(
        lifespan=lifespan,
        routes=[*create_agent_card_routes(card), *create_jsonrpc_routes(handler, "/", enable_v0_3_compat=True)],
    )


if __name__ == "__main__":
    uvicorn.run(create_app(), host="127.0.0.1", port=8080)
