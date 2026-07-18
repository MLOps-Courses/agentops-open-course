---
name: agent-incident-response
description: Run the operational loop for an LLM agent that is itself a production workload — detect, triage, mitigate without a redeploy, review blamelessly, and convert each incident into a permanent deterministic gate. Use when an agent in production burns its error budget, regresses latency or cost, or misbehaves, and you need a repeatable on-call response.
---

# Agent Incident Response

An operated agent is a running workload with incidents of its own: error-budget burn, latency regression, injection spikes, schema failures, cost blowouts, thumbs-down clusters. Close each one not when service is restored, but when a deterministic check would catch it again.

## When to use

- An agent workload fires an alert (error budget, p95, cost, guardrail spike, schema failure).
- A quality or cost incident with no alert (thumbs-down cluster, a change that doubled tokens).
- You want every outage to leave behind a test, eval case, alert, or baseline.

## The loop: detect → triage → mitigate → review → prevent

1. **Detect.** Alert on the workload's own health signals (error-budget burn, p95 latency, injection-neutralized spike, structured-output schema failures, missing token telemetry, collector down).
1. **Triage a fixed walk.** Metric (scope it) → trace (read the failing turn's span tree) → logs (the error text the span summarizes) → audit (what state changed, who approved). They join on the trace id.
1. **Mitigate without a redeploy.** Reach for runtime levers first: freeze writes with the kill-switch, roll back the prompt to a pinned version, cap the token/context envelope, disable a misbehaving opt-in or fail over the model. Record which lever and when — that timestamp is the first postmortem line.
1. **Review blamelessly.** Short, factual, about the system: impact, timeline, root cause tied to evidence, "what caught it and what didn't", actions with owners.
1. **Prevent — the load-bearing step.** Promote the incident to the cheapest deterministic gate that would catch it again: a new eval case, a new alert rule, a recommitted cost baseline, or a new red-team/unit test. The incident is closed only when that gate is green on main.

## Reference implementation

From the AgentOps Open Course:

- Course chapter `7.7. Incident Response` (the full loop and a postmortem template).
- Runtime levers across `4.5. Guardrails`, `4.4. Evaluations`, `7.3. Costs`.

## Verify

Induce one incident on your local stack (e.g. set the token ceiling to 1 and send a turn), walk the signals, write the five-section postmortem, and promote one action to a gate that runs without you — then confirm it is green.
