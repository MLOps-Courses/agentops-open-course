"""Run the model-backed ADK evaluation for the structured report entry point."""

from __future__ import annotations

import asyncio

from google.adk.evaluation.agent_evaluator import AgentEvaluator
from google.adk.evaluation.metric_evaluator_registry import DEFAULT_METRIC_EVALUATOR_REGISTRY
from google.adk.evaluation.metric_info_providers import TrajectoryEvaluatorMetricInfoProvider

from evals.governed_adk_eval import install_app_policy
from evals.required_trajectory import RequiredToolTrajectoryEvaluator
from evals.runtime import isolated_state, require_attributable_runtime


def main() -> None:
    """Evaluate schema-enforced reports with the same engine as ``adk eval``."""
    require_attributable_runtime()
    install_app_policy()
    # AgentEvaluator does not register config-declared custom metrics itself,
    # so install the same locked trajectory contract used by the ADK CLI.
    DEFAULT_METRIC_EVALUATOR_REGISTRY.register_evaluator(
        TrajectoryEvaluatorMetricInfoProvider().get_metric_info(),
        RequiredToolTrajectoryEvaluator,
    )
    with isolated_state("agentops-report-eval-"):
        asyncio.run(
            AgentEvaluator.evaluate(
                agent_module="agent.structured_report.agent",
                eval_dataset_file_path_or_dir="evals/triage-report.evalset.json",
                num_runs=1,
            )
        )


if __name__ == "__main__":
    main()
