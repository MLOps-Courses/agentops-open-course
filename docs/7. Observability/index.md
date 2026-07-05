---
description: Gain insight into the agent in production: reproducibility, tracing, monitoring, cost, feedback, online evaluation, and governance.
---

# 7. Observability

Your Ops Copilot now builds, runs, and deploys ([Chapter 6](../6. Platform/)). This chapter is the last turn of the [AgentOps loop](../0. Overview/0.2. AgentOps.md): keeping it healthy in production. You make runs reproducible, trace them with OpenTelemetry, monitor SLOs, track token cost, capture user feedback, evaluate live traffic, and govern the actions the agent takes.

Observability closes the loop back to the start: what you learn here — a regressed eval, a cost spike, a thumbs-down — becomes the next change to the prompt, the tools, or the guardrails. An agent that acts in the world is only trustworthy if you can see what it did and why.

This chapter covers:

- **[7.0. Reproducibility](./7.0. Reproducibility.md)**: Pin the version, model, and prompt; replay runs deterministically.
- **[7.1. Tracing](./7.1. Tracing.md)**: OpenTelemetry spans, `adk web` traces, and exporting to a collector.
- **[7.2. Monitoring](./7.2. Monitoring.md)**: Metrics, alerting, and SLOs for a tool-using agent.
- **[7.3. Costs](./7.3. Costs.md)**: Token and cost tracking, budgets, and KPIs.
- **[7.4. Feedback](./7.4. Feedback.md)**: Capture and route user feedback back into evals, skills, and runbooks.
- **[7.5. Online Evaluation](./7.5. Online Evaluation.md)**: Production evals, sampling live traffic, and drift detection.
- **[7.6. Governance](./7.6. Governance.md)**: Human-in-the-loop approvals, policy, the audit log, safety, and compliance.
