---
name: agent-incident-response
description: Run the operational loop for an LLM agent that is itself a production workload — detect, triage, mitigate with existing controls, review blamelessly, and convert each incident into a durable regression check. Use when an agent in production burns its error budget, regresses latency or cost, or misbehaves, and you need a repeatable on-call response.
---

# Agent Incident Response

An operated agent is a running workload with incidents of its own: error-budget burn, latency regression, injection spikes, schema failures, cost blowouts, and reviewed quality regressions. Close each one only when the smallest repeatable check or evidence path would expose it again.

## When to use

- An agent workload fires an alert (error budget, p95, cost, guardrail spike, schema failure).
- A quality or cost incident with no alert, such as repeated reviewer findings or doubled token usage.
- You want every outage to leave behind a test, eval case, alert, or baseline.

## The loop: detect → triage → mitigate → review → prevent

1. **Detect.** Alert on the workload's own health signals (error-budget burn, p95 latency, injection-neutralized spike, structured-output schema failures, missing token telemetry, collector down).
1. **Triage a fixed walk.** Metric (scope it) → trace (read the failing turn's span tree) → logs (the error text the span summarizes) → audit (what state changed, who approved). They join on the trace id.
1. **Mitigate with existing controls.** Roll out startup configuration to freeze writes, cap tokens/context, disable a misbehaving opt-in, or select a validated fallback model. Roll committed prompt or code changes back with the prior evaluated image digest. Record the replacement process's readiness time as the first postmortem line.
1. **Review blamelessly.** Short, factual, about the system: impact, timeline, root cause tied to evidence, "what caught it and what didn't", actions with owners.
1. **Prevent — the load-bearing step.** Promote the incident to the cheapest reproducible check or evidence path that would catch it again: a deterministic unit/red-team test, an eval case, an alert rule, or a corrected cost measurement. Fix or explicitly approve the cost change before reviewing a new baseline; never normalize unexplained inflation.

## Reference implementation

From the [AgentOps Open Course](https://agentops-open-course.fmind.dev/), installable with `npx skills add MLOps-Courses/agentops-open-course`:

- Course chapter `7.7. Incident Response` (the full loop and a postmortem template).
- `agents/go/policy/`, `agents/go/state/`, and `evals/` for startup controls, recovery, and regression evidence.
- Rollback controls across `4.5. Guardrails`, `7.0. Reproducibility`, and `7.3. Costs`.

## Verify

Induce one incident on your local stack (e.g. set the token ceiling to 1, restart, and send a turn), walk the signals, write the five-section postmortem, and promote one action to a repeatable check or evidence path — then confirm it produces the expected result.
