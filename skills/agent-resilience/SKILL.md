---
name: agent-resilience
description: Make an LLM agent's tool and model calls survive flaky and dead dependencies with deadlines, bounded retries, a circuit breaker, and a validated model fallback. Use when an agent hangs on a slow tool, storms a down gateway with retries, or has no failover when its model endpoint is unreachable.
---

# Agent Resilience

Give an agent's outbound calls three layers of failure handling, applied in the right place. The rule that generalizes: retry only where the call is **idempotent**, and only where a replica exists to retry against.

## When to use

- A slow tool or model call can hang a whole turn instead of failing on a deadline.
- A hard-down dependency is hammered with the full retry budget on every turn.
- The primary model endpoint has no failover, so its outage is a total outage.
- You are about to add a retry to a **write** — stop and read step 2 first.

## Steps

1. **Bound every call with a deadline.** Wrap each tool/model call so exceeding a timeout raises a clear error instead of hanging. Run sync tools in a worker thread so the deadline can actually fire on the event loop.
1. **Retry only idempotent reads, never writes.** Retrying a non-idempotent action (restart, resolve, charge) can apply it twice. Retry reads with bounded, exponential backoff; leave writes to run exactly once behind human confirmation.
1. **Add a circuit breaker for dead dependencies.** After N consecutive failures, open the breaker and fail fast (shed load) until a cooldown elapses, then let one trial call test recovery. Keep it opt-in so default behavior stays retry-only, and emit a metric each time it opens.
1. **Fail over the model only to a validated fallback.** Try a primary model; on a failure _before any response_, fall back to a secondary you have evaluated. Never switch mid-stream (it splices two answers), and prefer a smaller same-provider model so failover keeps your cost and privacy properties.

## Reference implementation

This skill is distilled from the AgentOps Open Course, which ships a working version:

- `resilience.py` — `with_resilience` (deadline + bounded retry, never on writes).
- `circuit.py` — a deterministic, clock-injectable `CircuitBreaker`.
- `model.py` — `FallbackLlm` wrapping a primary and a validated fallback.
- Course chapters `4.5. Guardrails` and `5.4. Model Gateway`.

## Verify

Write deterministic unit tests with an injected clock and fake failing calls: assert the breaker opens after the threshold, fails fast while open, and recovers half-open; assert a write is never wrapped; assert the fallback engages only when the primary fails before responding.
