---
name: agent-least-privilege
description: Split an LLM agent into a coordinator plus least-privilege specialists so that a prompt injection cannot reach a tool it was never given, containing blast radius by construction rather than by instruction. Use when one agent holds both untrusted-content readers and consequential write tools, or when you are deciding whether to split an agent at all.
---

# Agent Least Privilege

Split work by **authority**, not because a diagram looks tidier. A boundary a model could talk itself across is prose; a boundary enforced by which tools an agent physically holds is policy.

## When to use

- One agent holds both readers of attacker-influenceable content and consequential write tools.
- You want an injected instruction to have nothing to call, by construction.
- You are unsure whether to split an agent or keep it single (see "When one agent is better").

## Steps

1. **Separate reading from acting.** Give the agent that reads untrusted content (logs, web, tool output) _no_ write tools; give the agent that can act _no_ raw-content readers. An injection reaching the reader then has nothing to invoke.
1. **Route by description, and keep policy in the coordinator.** A coordinator delegates to specialists whose `description` fields the model matches the task against — write them precisely. Keep the _policy_ (diagnose before remediate) in the coordinator's instruction, not in the model's discretion.
1. **Keep writes gated even after the split.** Least privilege limits _capability_, not _content_: the injected text still circulates in shared history. So the acting agent's writes still require human approval, and you still spotlight untrusted output.
1. **Share one budget.** Delegation multiplies model calls; carry one shared token ceiling across all sub-agents so a chain that keeps hopping still hits a single limit.

## When one agent is better

Most of the time. Every delegation is an extra model call and another candidate author for a wrong answer. Split only when tool sets must differ in authority, one instruction has grown contradictory, or a sub-task needs a different model/budget. Otherwise prefer one agent with well-guarded tools.

## Reference implementation

From the AgentOps Open Course:

- `delegation.py` — a coordinator with read-only and write-only specialists.
- Course chapter `3.7. Multi-Agent`.

## Verify

Assert in a test that each specialist physically lacks the other's tools (the read-only agent holds no write tool; the acting agent holds only the guarded writes), so the boundary is structural, not prompted.
