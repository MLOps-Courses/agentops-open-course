"""Run the model-backed ADK evaluation for the structured report entry point."""

from __future__ import annotations

import asyncio

from google.adk.evaluation.agent_evaluator import AgentEvaluator


def main() -> None:
    """Evaluate schema-enforced reports with the same engine as ``adk eval``."""
    asyncio.run(
        AgentEvaluator.evaluate(
            agent_module="agent.structured_report.agent",
            eval_dataset_file_path_or_dir="evals/triage-report.evalset.json",
        )
    )


if __name__ == "__main__":
    main()
