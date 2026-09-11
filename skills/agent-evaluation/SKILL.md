---
name: agent-evaluation
description: Build offline checks and model-backed evidence for an LLM agent — trajectory scoring, groundedness/citation coverage, a run-over-run token drift warning, and side-by-side prompt A/B. Use when a prompt or model change might silently regress behavior, cost, or grounding, or when "it looked fine" is your only test.
---

# Agent Evaluation

Score an agent's _behavior_ over fixed cases, not one exact string. Let model-free structure checks gate merges. Treat every model-backed scorer as evidence with an explicit floor, and never make an LLM judge the sole release criterion.

## When to use

- A prompt/model change can pass a smoke test yet call the wrong tools, cost more, or hallucinate.
- You want deterministic evalset validation in pull requests and behavioral evidence before release.
- You need to choose between two prompt versions with numbers, not opinion.

## Steps

1. **Score the trajectory, not the wording.** Assert which tools were called, with which arguments, in order (allow extra reads) over fixed seed cases; hold _writes_ to an exact count. This survives non-determinism that exact-match scoring cannot.
1. **Grow the set from real failures.** When a trace shows a wrong or unsafe trajectory, distil it into one case that pins that single behavior and makes a recurrence visible.
1. **Add a groundedness check.** Require every recognized claim to appear in that turn's retrieved evidence or the user's question. Document the recognizer's vocabulary; use a broader extractor or judge for claims it cannot parse.
1. **Warn on token drift.** Total this run's tokens and model calls, compare them with the previous run of the same evalset and model, and print the change when it exceeds a stated tolerance — 25% in the reference implementation. Keep it a warning, not a gate: tokens move for honest reasons, and a run that answered every case correctly should not fail for spending more to do it. Trajectory scores tolerate waste, so this is the only signal that surfaces a correct-but-expensive change at all.
1. **A/B prompt versions.** Run the eval set under two pinned prompt versions in isolated processes and print a per-scorer delta; promote or roll back on the numbers.
1. **Split gates from evidence.** Deterministic, model-free checks gate CI; model-backed evaluations run as commit-scoped evidence a human reads before release, with the thresholds visible on the command that produced them.

## Reference implementation

From the [AgentOps Open Course](https://agentops-open-course.fmind.dev/), installable with `npx skills add MLOps-Courses/agentops-open-course`:

- `evals/turn.go` and `evals/client.go` — typed REST/A2A capture and partial-safe usage folding.
- `evals/trajectory.go`, `evals/groundedness.go`, and `evals/schema.go` — deterministic scorers; `evals/drift.go` — the run-over-run token drift warning.
- `evals/judge.go` and `evals/evidence.go` — calibrated gateway verdicts and sanitized OTel evidence.
- Course chapter `4.4. Evaluations`.

## Verify

Introduce a deliberate regression (a wrong tool, a fabricated id) and confirm the matching scorer turns red; revert and confirm green. Double a run's token count and confirm the drift line prints without failing the run — that one is a warning by design.
