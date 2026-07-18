---
name: agentops-telemetry
description: Instrument an LLM agent with OpenTelemetry traces, metrics, and logs so you can see why a turn behaved as it did, with user content kept out of spans by default. Use when an agent is a black box in production, when you need per-turn traces of model and tool calls, or when wiring an agent to MLflow, Prometheus, Grafana, or Loki.
---

# AgentOps Telemetry

Trace an agent the way you trace a distributed system: one correlated record of model, tool, and application work per request — not a log line with the final answer. Make privacy the default, not an afterthought.

## When to use

- An agent misbehaves in production and you can only see the final output.
- You need to correlate a model call, its tool calls, tokens, and latency for one turn.
- You are exporting agent telemetry to MLflow (traces), Prometheus (metrics), or Loki (logs).
- You must instrument without leaking user prompts or model responses into span storage.

## Steps

1. **Emit spans over OpenTelemetry, using the GenAI semantic conventions.** Name attributes with the standard `gen_ai.*` keys (`gen_ai.operation.name`, `gen_ai.request.model`, `gen_ai.tool.name`) and `error.type` so any OTel backend understands them.
1. **Keep content capture off by default.** Traces should carry timing, model, tool, token, and status metadata — not the prompt or response body. Make capturing content an explicit, auditable opt-in with a stated privacy and retention cost.
1. **Export through the OTel Collector, then fan out.** Send OTLP to a collector that routes traces to your trace store, derives request-count/latency metrics from spans (a spanmetrics connector), and ships logs to a log store — one pipeline, many backends.
1. **Redact and bound the log bridge.** If you bridge application logs to OTLP, redact secrets/PII and cap size before export, and deduplicate noisy lines.
1. **Derive RED metrics from spans.** Rate, Errors, Duration over a bounded label set (operation, model, error type) — never label by prompt, user, session, or trace id, which explodes cardinality.

## Reference implementation

From the AgentOps Open Course:

- `telemetry.py` — OTLP setup and a redacting, bounded, dedup log bridge; content capture off by default.
- `infra/observability/` — OTel Collector, Prometheus, Grafana, Loki, and a shipped dashboard.
- Course chapters `7.1. Tracing` and `7.2. Monitoring`.

## Verify

Run one turn, open its trace, and confirm the span tree shows the model and tool spans with token/latency attributes and **no** prompt or response text; confirm a metric counter moved and a dashboard panel updated.
