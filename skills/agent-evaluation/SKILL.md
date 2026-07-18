---
name: agent-evaluation
description: Build offline, deterministic evaluations for an LLM agent — trajectory scoring, groundedness/citation coverage, a token-cost regression tripwire, and side-by-side prompt A/B — that gate changes instead of vibes. Use when a prompt or model change might silently regress behavior, cost, or grounding, or when "it looked fine" is your only test.
---

# Agent Evaluation

Score an agent's _behavior_ over fixed cases, not one exact string. Keep the deterministic scorers as the gate and any LLM judge as evidence only — a non-deterministic judge must never be the sole release criterion.

## When to use

- A prompt/model change can pass a smoke test yet call the wrong tools, cost more, or hallucinate.
- You want a regression gate that fails a pull request before a bad change ships.
- You need to choose between two prompt versions with numbers, not opinion.

## Steps

1. **Score the trajectory, not the wording.** Assert which tools were called, with which arguments, in order (allow extra reads) over fixed seed cases; hold _writes_ to an exact count. This survives non-determinism that exact-match scoring cannot.
1. **Grow the set from real failures.** When a trace shows a wrong or unsafe trajectory, distil it into one case that pins that single behavior, so the regression can never silently return.
1. **Add a groundedness check.** For each answer, require every entity it names (ids, names, categories) to appear in that turn's retrieved evidence or the user's question — catches hallucinated entities a correctness check would wave through.
1. **Tripwire the cost.** Record per-case tokens and model-call counts against a committed baseline; fail when a change exceeds a tolerance. Trajectory scores tolerate waste, so this is the only thing that catches a correct-but-expensive regression.
1. **A/B prompt versions.** Run the eval set under two pinned prompt versions in isolated processes and print a per-scorer delta; promote or roll back on the numbers.
1. **Split gates from evidence.** Deterministic, model-free checks gate CI; model-backed evals (judge, cost, groundedness) run on a schedule where a failure means "investigate", not "blocked".

## Reference implementation

From the AgentOps Open Course:

- `evals/mlflow_eval.py` — deterministic trajectory/response scorers + optional judge.
- `evals/groundedness_eval.py`, `evals/cost_eval.py`, `evals/prompt_ab.py`.
- Course chapter `4.4. Evaluations`.

## Verify

Introduce a deliberate regression (a wrong tool, a doubled token count, a fabricated id) and confirm the matching scorer turns red; revert and confirm green.
