---
name: agent-token-budget
description: Bound an LLM agent's per-session token spend and attribute cost, so a runaway loop ends with an actionable message instead of an open-ended bill. Use when an agent's multi-step loop can spend without limit, when you need per-session token accounting, or when a delegation chain could multiply model calls.
---

# Agent Token Budget

Every agent loop step is another model call, so cost and latency compound. Give each conversation a hard token ceiling and attribute usage to the session — bounding reasoning work, not just dollars.

## When to use

- An agentic loop or delegation chain can call the model an unbounded number of times.
- You need to answer "how many tokens did this session use?" and "what happens at the limit?".
- You want token/cost signals in traces and metrics without hardcoding a vendor's prices.

## Steps

1. **Accumulate usage into session state.** After each model response, add its input/output tokens to a running per-session total that persists across turns (no `temp:` prefix if your session store honors one).
1. **Enforce a hard ceiling before the model call.** In a before-model hook, if the session total has reached the limit, short-circuit with a clear message ("start a new session, or raise the limit") instead of a silent failure or an open-ended bill.
1. **Attribute, don't just count.** Emit tokens as an OTel counter (graph throughput) and as span attributes (per-turn detail); compute cost from configurable per-1k prices that default to 0 for local models — never hardcode a provider's pricing.
1. **Make the budget cover the whole conversation.** In multi-agent/delegation flows, keep the totals in shared session state so hops between sub-agents accumulate against one ceiling rather than each starting fresh.

## Reference implementation

From the AgentOps Open Course:

- `budget.py` — `record_token_usage`, `enforce_token_budget`, `estimate_cost`; OTel counter + span attributes.
- Course chapters `7.3. Costs` and `3.7. Multi-Agent`.

## Verify

Set the ceiling to 1 token and send a turn: assert the model call is refused with the actionable message and the token counter reflects the usage; confirm a delegation chain hits one shared ceiling, not one per sub-agent.

## Honest limits

A per-session budget bounds one conversation, not one client — a caller who rotates sessions bypasses it, so pair it with a per-client/tenant limit at the gateway. It bounds tokens, not dollars, unless you supply real prices.
