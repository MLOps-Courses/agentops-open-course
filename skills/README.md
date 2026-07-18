# AgentOps Skills

Installable [Agent Skills](https://agents.md/) that package this course's operational patterns so you can apply them in **your own** agent projects. They are tool-agnostic guidance — the how and the why — each pointing back to the worked reference implementation in this repository.

These are distinct from `agents/data/skills/`, which are runtime skills the reference agent loads at execution time. The skills here are for the human (or coding agent) building and operating an agent.

## Install

With the [`skills` CLI](https://github.com/vercel-labs/skills) (works with Antigravity, Codex, OpenCode, Claude, and Copilot):

```bash
npx skills add MLOps-Courses/agentops-open-course --all   # all of them
npx skills add MLOps-Courses/agentops-open-course --skill agent-resilience   # just one
```

The CLI auto-discovers every `SKILL.md` under this `skills/` directory. Review a skill's source before installing — skill text runs with your agent's permissions.

## The skills

| Skill                                                           | Covers                                                                 |
| --------------------------------------------------------------- | ---------------------------------------------------------------------- |
| [`agentops-course`](./agentops-course/SKILL.md)                 | Index of the patterns and how they fit together.                       |
| [`agentops-telemetry`](./agentops-telemetry/SKILL.md)           | OpenTelemetry traces, metrics, logs; content off by default.           |
| [`agent-guardrails`](./agent-guardrails/SKILL.md)               | PII redaction, injection spotlighting, HITL approval, a kill-switch.   |
| [`agent-resilience`](./agent-resilience/SKILL.md)               | Deadlines, bounded retries, circuit breaker, validated model fallback. |
| [`agent-token-budget`](./agent-token-budget/SKILL.md)           | Per-session token ceilings and cost attribution.                       |
| [`agent-least-privilege`](./agent-least-privilege/SKILL.md)     | Least-privilege specialists that contain prompt injection.             |
| [`agent-evaluation`](./agent-evaluation/SKILL.md)               | Trajectory, groundedness, and cost-regression evaluations.             |
| [`agent-incident-response`](./agent-incident-response/SKILL.md) | The detect→triage→mitigate→review→prevent loop for an agent workload.  |

Each skill closes with a "Reference implementation" section naming the exact course files it distills, so you can read a real, tested version alongside the guidance.
