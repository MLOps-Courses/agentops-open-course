---
description: "Make the agent correct and trustworthy: typing, linting, testing, metrics, evaluations, guardrails, and security."
---

# 4. Quality

!!! abstract "In one glance"

    - **You will:** See how the chapter's seven quality layers fit together and which page owns each check.
    - **You need:** Chapter 3 finished and `mise run test` passing.
    - **Time:** about 5 minutes, orientation.

**Part I — Agent development.** Work through [2.6. Workshop](../2.%20Agents/2.6.%20Workshop.md) and consult this chapter when the exercise introduces its subject. Python fundamentals are assumed; no Kubernetes knowledge is needed here.

## How will you make the agent trustworthy?

Trust comes in layers. Each page below adds one, and each layer catches a class of failure the cheaper layers below it cannot.

Your agent now holds a conversation ([Chapter 2](../2. Agents/)) and has bounded capabilities ([Chapter 3](../3. Capabilities/)). This chapter makes it defensible.

The early checkpoints need no model, account, network, or bill. The full maintainer security gate may refresh advisory data. The marker on each line says what that page's own checkpoint needs; run `mise run doctor:model` before a model-backed one.

- **[4.0. Typing](./4.0. Typing.md)** _(concept · offline)_: Python typing with ty, parsing tool I/O at the boundary.
- **[4.1. Linting](./4.1. Linting.md)** _(hands-on · offline)_: Lint and format with ruff and dprint.
- **[4.2. Testing](./4.2. Testing.md)** _(hands-on · offline)_: Fast, offline unit tests with pytest, against an isolated dataset copy.
- **[4.3. Metrics](./4.3. Metrics.md)** _(reference · needs a model)_: A scorecard of deterministic gates, model-backed evidence, and observed operational indicators.
- **[4.4. Evaluations](./4.4. Evaluations.md)** _(hands-on · needs a model)_: ADK trajectories plus full-conversation MLflow lineage and optional judge evidence.
- **[4.5. Guardrails](./4.5. Guardrails.md)** _(hands-on · offline, except the last checkpoint step)_: Boundary redaction, stable errors, confirmation, transactions, and audit evidence.
- **[4.6. Security](./4.6. Security.md)** _(hands-on · model-free; scans may use network)_: Threat modeling, offline adversarial regressions, identity, and supply-chain scanning.

Expect to write code, not just read. [4.5. Guardrails](./4.5. Guardrails.md) carries the chapter's required `## Your turn` drill — turn a guardrail into a test that fails if it ever weakens — and [4.4. Evaluations](./4.4. Evaluations.md) has you add an eval case on top of it.

## Where is gate versus evidence explained?

[4.7. Evaluation Reference](./4.7.%20Evaluation%20Reference.md#which-evaluation-task-should-you-run-and-when) owns the definition, workflow map, and task-by-task decision. This index only marks each page's prerequisites so you can enter the chapter without learning the same policy twice.

- **[4.7. Evaluation Reference](./4.7. Evaluation Reference.md)** _(reference)_: Advanced evaluation, model judges, prompt registries, and measurement details.

- **[4.8. Developer Handoff](./4.8. Developer Handoff.md)** _(hands-on)_: Complete Part I and check readiness for Kubernetes platform engineering.

## What proves this chapter worked?

Two offline commands cover the chapter: the full test suite, then the adversarial regression suite.

```bash
cd agents/python
mise run test
mise run redteam
```

Neither needs a model, a provider key, or a network.

**You are done when:**

- `mise run test` passes, including the enforced 95% combined line-and-branch coverage floor.
- `mise run redteam` passes every adversarial case in `tests/test_security.py`.
- The chapter's required drill is done: the `## Your turn` in [4.5. Guardrails](./4.5.%20Guardrails.md#your-turn-how-do-you-turn-a-guardrail-into-a-regression) added a regression you watched fail against a deliberately weakened guard, then restored.
- You can use the page markers above to say which checkpoints need a configured model and which run offline.
- You can point to [4.7. Evaluation Reference](./4.7.%20Evaluation%20Reference.md#which-evaluation-task-should-you-run-and-when) for the chapter's gate-versus-evidence policy.
- Without reopening Chapter 3: you can name which of the six memory stores a value belongs in when it must survive the next turn but not the next session.

Continue to [4.0. Typing](./4.0.%20Typing.md) when you know the first three pages need no model or provider account.
