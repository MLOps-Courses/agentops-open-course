---
name: agentops-course
description: Index of the AgentOps patterns for operating LLM agents in production — telemetry, guardrails, resilience, evaluation, token budgets, least privilege, and incident response — with a pointer to the full open-source course. Use when you want an overview of how the AgentOps skills fit together, or where to start operating an agent you have built.
---

# AgentOps Patterns

Getting an agent to answer correctly once is a demo. **AgentOps** is keeping it correct, safe, affordable, and observable as it runs against real traffic. These skills are operational patterns extracted from one completed reference agent, each with a file you can read and a repeatable check or evidence path.

## When to use

- You have built an agent and now have to _operate_ it.
- You want to know which AgentOps skill covers a given concern.
- You want the worked, open-source reference behind these patterns.

## The patterns

Each pattern is a sibling skill, and installing this index alone does not put any of them on disk. They ship from the same package, so add the ones your agent needs by name:

```bash
npx skills add MLOps-Courses/agentops-open-course --skill agent-resilience
```

1. **`agentops-telemetry`** — trace, meter, and log the agent with OpenTelemetry; content off by default.
1. **`agent-guardrails`** — PII redaction, injection spotlighting, human approval on writes, a kill-switch.
1. **`agent-resilience`** — deadlines, bounded retries, a circuit breaker, and a validated model fallback.
1. **`agent-token-budget`** — per-session token ceilings and cost attribution.
1. **`agent-least-privilege`** — split into least-privilege specialists so injection has nothing to call.
1. **`agent-evaluation`** — deterministic validation gates plus model-backed trajectory, grounding, and cost evidence.
1. **`agent-incident-response`** — the detect→triage→mitigate→review→prevent loop for the agent as a workload.

## How they fit together

Build and instrument first (telemetry), harden the boundaries (guardrails, least privilege), bound the failure modes (resilience, token budget), collect behavior evidence (evaluation), and operate the running system (incident response) — feeding every real failure back into the smallest repeatable check or evidence path.

## The full course

These patterns are distilled from the **AgentOps Open Course**, which builds, evaluates, secures, deploys, and operates one production-shaped Go agent with Google ADK, agentgateway, kagent, OpenTelemetry, Tempo, Loki, Prometheus, Grafana, and Ollama. Read it at <https://agentops-open-course.fmind.dev/>; its `README.md` states the current proof boundaries.
