---
description: Make the agent correct and trustworthy: typing, linting, testing, metrics, evaluations, guardrails, and security.
---

# 4. Quality

Make the agent correct and trustworthy: typing, linting, testing, metrics, evaluations, guardrails, and security. A working agent is not a finished one — this chapter is the discipline that turns a demo into something you would put on call. It moves from the deterministic foundations (types, lint, tests) to agent-specific quality (the metrics that matter and the evals that gate them) and finally to safety (guardrails and security). Every gate runs through the same `mise run` tasks that git hooks and CI use, so quality is enforced automatically, not by hope.

This chapter covers:

- **[4.0. Typing](./4.0. Typing.md)**: Python typing with ty and Go static types, parsing tool I/O at the boundary.
- **[4.1. Linting](./4.1. Linting.md)**: Lint and format with ruff, golangci-lint, and dprint.
- **[4.2. Testing](./4.2. Testing.md)**: Fast, offline unit tests with pytest and gotestsum, against an isolated dataset copy.
- **[4.3. Metrics](./4.3. Metrics.md)**: The metrics that define agent quality — task success, tool trajectory, groundedness, latency, cost, and safety.
- **[4.4. Evaluations](./4.4. Evaluations.md)**: `adk eval` and tool-trajectory gating over a fixed dataset.
- **[4.5. Guardrails](./4.5. Guardrails.md)**: Safety callbacks, input validation, and human-in-the-loop approval.
- **[4.6. Security](./4.6. Security.md)**: Secrets, prompt injection, and dependency and container scanning.
