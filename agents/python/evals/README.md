# Evaluations (Python)

Evaluation sets for the Ops Copilot, run with `adk eval` (Chapter 4.4).

## Run

```bash
mise run eval        # adk eval src/agent evals/ops.evalset.json --config_file_path evals/test_config.json
```

## Files

- `ops.evalset.json` — recorded eval cases. Each case fixes a user prompt, the expected **tool trajectory** (which tools with which arguments), and a reference final response.
- `test_config.json` — the pass criteria. We gate on **`tool_trajectory_avg_score: 1.0`**: the agent must call exactly the right tools with the right arguments — a deterministic, meaningful contract over the fixed dataset.

`response_match_score` (ROUGE overlap with the reference text) is intentionally **not** a gate: generative wording varies run to run, so it is a soft signal, not a pass/fail criterion.

Evals call the live model, so they are a manual/CI-optional gate — they are **not** part of `mise run test` (which stays fast and offline). Add more `*.evalset.json` files here as the agent grows.
