# AgentOps Skills

Installable [Agent Skills](https://agentskills.io/specification) that package this course's operational patterns so you can apply them in **your own** agent projects. They are tool-agnostic guidance — the how and the why — each pointing back to the worked reference implementation in this repository.

These are distinct from `agents/data/skills/`, which are runtime skills the reference agent loads at execution time. The skills here are for the human (or coding agent) building and operating an agent.

## Install

With the [`skills` CLI](https://github.com/vercel-labs/skills) (works with Antigravity, Codex, OpenCode, Claude, and Copilot):

```bash
npx skills add MLOps-Courses/agentops-open-course --all   # all of them
npx skills add MLOps-Courses/agentops-open-course --skill agent-resilience   # just one
```

The CLI auto-discovers every `SKILL.md` under this `skills/` directory. Review a skill's source before installing — skill text runs with your agent's permissions.

## Which skill covers what

[`agentops-course`](./agentops-course/SKILL.md) is the index: it names all seven patterns, says what each one covers, and explains the order they compose in. It is a skill rather than a table here because it also ships — an agent that installed only it can still route to the right sibling, which a repository README cannot do. Keeping the list in one place means adding a skill is one edit, not two.

Each skill closes with a "Reference implementation" section naming the exact course files it distills, so you can read a real, tested version alongside the guidance.
