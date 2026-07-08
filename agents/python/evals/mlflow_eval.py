"""MLflow GenAI evaluation for the Ops Copilot (Chapters 4.4 and 7.0).

Complements ``adk eval`` — which gates the exact tool trajectory — with what MLflow adds and ADK
does not: a **persistent experiment + comparison UI** (``mlflow ui``), a versioned **Prompt
Registry** entry for the agent instruction (7.0 Reproducibility), and optional **LLM-as-judge**
scoring of answer quality. It reuses the *same* cases as ``ops.evalset.json`` so the two evals stay
in lockstep.

Run it with ``mise run eval:mlflow``:
  * the agent runs live, so it needs a provider key (``GOOGLE_API_KEY``), like ``adk eval``;
  * the trajectory scorer is code-only (no judge, no extra key);
  * the LLM judges are added only when ``MLFLOW_JUDGE_MODEL`` is set (e.g. ``gemini:/gemini-2.5-flash``),
    keeping the judge on your own provider — MLflow uses LiteLLM to reach it as a *dev-time eval*
    detail; the agent itself still never uses LiteLLM (it reaches providers via the gateway, Ch. 5).

View results: ``uv run mlflow ui`` → open http://localhost:5000 and pick the ``ops-copilot`` experiment.
"""

from __future__ import annotations

import asyncio
import json
import os
from pathlib import Path
from typing import Any

import mlflow
import mlflow.genai
from google.adk.runners import InMemoryRunner
from google.genai import types
from mlflow.genai.scorers import Scorer, scorer

from agent.agent import INSTRUCTION, root_agent

_EVALSET = Path(__file__).parent / "ops.evalset.json"
# Local, zero-server tracking store. Prompt Registry needs a SQL backend, so we use sqlite (not a file store).
_TRACKING_URI = os.environ.get("MLFLOW_TRACKING_URI", f"sqlite:///{Path(__file__).parent / 'mlflow.db'}")


def _load_cases() -> list[dict[str, Any]]:
    """Parse the shared ADK eval set into MLflow rows: inputs (the question) + expectations."""
    evalset = json.loads(_EVALSET.read_text())
    rows: list[dict[str, Any]] = []
    for case in evalset["eval_cases"]:
        turn = case["conversation"][0]
        rows.append(
            {
                "inputs": {"question": turn["user_content"]["parts"][0]["text"]},
                "expectations": {
                    "expected_tools": [use["name"] for use in turn["intermediate_data"]["tool_uses"]],
                    "expected_response": turn["final_response"]["parts"][0]["text"],
                },
            }
        )
    return rows


async def _run(question: str) -> dict[str, Any]:
    """Drive the agent once and return its final answer plus the tools it called, in order."""
    runner = InMemoryRunner(agent=root_agent, app_name="ops-copilot")
    session = await runner.session_service.create_session(app_name="ops-copilot", user_id="eval")
    message = types.Content(role="user", parts=[types.Part(text=question)])
    tools: list[str] = []
    answer = ""
    async for event in runner.run_async(user_id="eval", session_id=session.id, new_message=message):
        tools.extend(call.name for call in event.get_function_calls() if call.name)
        if event.is_final_response() and event.content and event.content.parts:
            answer = event.content.parts[0].text or ""
    return {"response": answer, "tools": tools}


def ask(question: str) -> dict[str, Any]:
    """Synchronous ``predict_fn`` for MLflow — one agent run per eval case."""
    return asyncio.run(_run(question))


@scorer
def tool_trajectory(outputs: dict[str, Any], expectations: dict[str, Any]) -> bool:
    """Code-only scorer (no LLM): the agent must call exactly the expected tools, in order.

    This mirrors ``adk eval``'s ``tool_trajectory_avg_score: 1.0`` gate — the deterministic contract.
    """
    return outputs.get("tools") == expectations.get("expected_tools")


def _scorers() -> list[Scorer]:
    """Trajectory scorer always; add LLM judges only when a judge model is configured."""
    scorers: list[Scorer] = [tool_trajectory]
    judge_model = os.environ.get("MLFLOW_JUDGE_MODEL")
    if judge_model:
        from mlflow.genai.scorers import Correctness, Guidelines

        scorers += [
            Correctness(model=judge_model),
            Guidelines(
                model=judge_model,
                name="grounded",
                guidelines="The answer must be grounded in the tool results and must not invent "
                "incident ids, services, or statuses.",
            ),
        ]
    return scorers


def main() -> None:
    mlflow.set_tracking_uri(_TRACKING_URI)
    mlflow.set_experiment("ops-copilot")
    # Version the persona as a first-class artifact so a run is traceable to the exact prompt (7.0).
    mlflow.genai.register_prompt(
        name="ops-copilot-instruction",
        template=INSTRUCTION,
        commit_message="Ops Copilot system instruction",
    )
    result = mlflow.genai.evaluate(data=_load_cases(), predict_fn=ask, scorers=_scorers())
    print("MLflow eval complete. Metrics:")  # noqa: T201 — a CLI entry point, printing is the point
    for name, value in result.metrics.items():
        print(f"  {name}: {value}")  # noqa: T201
    print(f"\nView the run: uv run mlflow ui --backend-store-uri {_TRACKING_URI}")  # noqa: T201


if __name__ == "__main__":
    main()
