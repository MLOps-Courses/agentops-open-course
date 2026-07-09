# Evaluations (Python)

Evaluation sets for the Ops Copilot, run with `adk eval` (Chapter 4.4).

## Run

```bash
mise run eval        # adk eval — gates the exact tool trajectory (needs a provider key)
mise run eval:mlflow # MLflow GenAI eval — same cases, adds LLM-judge answer scoring + a prompt registry
```

## Files

- `ops.evalset.json` — recorded eval cases. Each case fixes a user prompt, the expected **tool trajectory** (which tools with which arguments), and a reference final response.
- `test_config.json` — the pass criteria. We gate on **`tool_trajectory_avg_score: 1.0`**: the agent must call exactly the right tools with the right arguments — a deterministic, meaningful contract over the fixed dataset.
- `mlflow_eval.py` — the **[MLflow](https://mlflow.org/)** GenAI eval (Apache-2.0, self-hosted). Reuses `ops.evalset.json`, adds a code-only trajectory scorer plus optional LLM-as-judge scoring (`Correctness`, `Guidelines`) and a versioned prompt registry. Browse results with `uv run mlflow ui --backend-store-uri sqlite:///evals/mlflow.db` (from `agents/python/`, so the store path resolves). See [7.0. Reproducibility](../../../docs/7.%20Observability/7.0.%20Reproducibility.md).

`response_match_score` (ROUGE overlap with the reference text) is intentionally **not** a gate: generative wording varies run to run, so it is a soft signal, not a pass/fail criterion.

Evals call the live model, so they are a manual/CI-optional gate — they are **not** part of `mise run test` (which stays fast and offline). Add more `*.evalset.json` files here as the agent grows.
