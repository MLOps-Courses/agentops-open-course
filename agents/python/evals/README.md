# Ops Copilot Evaluations

The evaluation layer separates deterministic engineering gates from model-backed behavioral evidence. It never weakens the offline unit tests to accommodate non-deterministic output.

## Evaluation layers

| Layer                  | Command                  | Model required? | Gate                                                                                        |
| ---------------------- | ------------------------ | --------------- | ------------------------------------------------------------------------------------------- |
| Unit/integration       | `mise run test`          | No              | Exact typed behavior and at least 95% branch coverage.                                      |
| Adversarial regression | `mise run redteam`       | No              | Deterministic injection, boundary, and policy cases.                                        |
| Evalset consistency    | `mise run eval:validate` | No              | Cases reference committed seed entities; strict in-order trajectory criteria.               |
| ADK trajectory         | `mise run eval`          | Yes             | Expected tools and arguments over the fixed seed.                                           |
| MLflow evaluation      | `mise run eval:mlflow`   | Yes             | Complete turn/part capture, code scorers, prompt/model lineage, and optional judge scorers. |

## Run the live evaluations

From `agents/python/`, configure native Gemini or an OpenAI-compatible agentgateway model, then:

```bash
mise run eval
mise run eval:mlflow
```

The MLflow tracking URI defaults to local SQLite unless `MLFLOW_TRACKING_URI` is set; `MLFLOW_EXPERIMENT_NAME` defaults to `ops-copilot`. Chapter 7 starts the self-hosted server at `http://localhost:5000`. The command prints the authoritative destination and suggests a local `mlflow ui` command only for a `sqlite:` URI, never for an HTTP server.

## Configure an optional MLflow judge

The code-only scorers always run. An LLM judge is opt-in:

```bash
MLFLOW_JUDGE_MODEL=qwen3:4b
MLFLOW_JUDGE_BASE_URL=http://localhost:4000/v1
MLFLOW_JUDGE_API_KEY=agentgateway
```

`MLFLOW_JUDGE_BASE_URL` and `MLFLOW_JUDGE_API_KEY` fall back to the corresponding `OPENAI_*` values. No evaluation path uses LiteLLM.

Treat judge output as evidence, not truth. Record the judge model and prompt, inspect disagreement, and never use a single model score as the only release criterion.

## Files

- `ops.evalset.json` contains prompts, expected tool trajectories, and reference answers over the fixed dataset — happy paths plus deliberate negative and adversarial cases.
- `test_config.json` defines ADK pass criteria; the tool-trajectory score with `IN_ORDER` matching is the behavioral gate.
- `mlflow_eval.py` preserves every turn and part, registers the prompt, links prompt/model lineage to the run, applies deterministic scorers (same `IN_ORDER` semantics), and adds optional judge scorers.
- `../tests/test_evalset.py` is the offline consistency check behind `mise run eval:validate`: every referenced incident/service/runbook must exist in the committed seed, and the deliberate negatives (`INC-999`, `warehouse`) must stay missing.

ROUGE-style response overlap is intentionally not a hard gate because valid generative wording varies. Tool selection, arguments, policy decisions, and trusted data boundaries are stronger contracts.

## Write a good case

- **One behavior per case.** A case that checks lookup, diagnosis, and approval at once cannot tell you which behavior regressed; split it.
- **Negative cases matter as much as happy paths.** Unknown entities (`INC-999`, `warehouse`), actions that must wait for approval, and injected instructions in tool output are where an agent fails dangerously — an all-happy-path set would score a regression there as green.
- **Assert the trajectory, not the prose.** Expected tool names/arguments in order are stable across model versions and rewordings; `IN_ORDER` matching tolerates harmless extra calls (a memory recall, an incident list) without weakening the contract.
- **Grow the set from real failures.** When a trace shows a wrong tool choice or an unsafe proposal, distill it into the minimum conversation that reproduces it, then keep it forever as a regression case.

## Add a regression case

Use a stable incident from the committed seed, record the minimum necessary expected trajectory, and avoid credentials or real operational data. Run offline tests first (`test_evalset.py` catches dangling entity references immediately), then both live evaluation commands with an explicit model. Document model/version changes when comparing results over time.

See [4.4. Evaluations](../../../docs/4.%20Quality/4.4.%20Evaluations.md) and [7.0. Reproducibility](../../../docs/7.%20Observability/7.0.%20Reproducibility.md).
