---
description: Gain insight into the agent in production: reproducibility, tracing, monitoring, cost, feedback, online evaluation, and governance.
---

# 7. Observability

## How will you operate the agent after deployment?

Your Ops Copilot now runs as a private Kubernetes workload ([Chapter 6](../6. Platform/)). This chapter closes the [AgentOps loop](../0. Overview/0.2. AgentOps.md) with evidence: release lineage, OpenTelemetry traces, span-derived/gateway metrics, explicit cost assumptions, trace-linked human feedback, a safe online-evaluation design, and auditable approved actions.

The shipped OSS stack is MLflow, OpenTelemetry Collector, Prometheus, Alertmanager, Loki, and Grafana. Pages distinguish implemented signals from desired production extensions: there is no fake dollar-cost panel, automatic live judge, external paging integration (alerts stop at the local Alertmanager), cryptographically immutable audit store, or HA claim.

This chapter covers:

- **[7.0. Reproducibility](./7.0. Reproducibility.md)**: Version code, images, model path, prompt, data, tools, and evaluation evidence together.
- **[7.1. Tracing](./7.1. Tracing.md)**: Export privacy-preserving ADK/gateway spans through OTel into MLflow.
- **[7.2. Monitoring](./7.2. Monitoring.md)**: Query the shipped RED/gateway metrics, Loki agent logs, and provisioned Grafana dashboard, and respond to the shipped alerts.
- **[7.3. Costs](./7.3. Costs.md)**: Bound model work and state conditional local/GKE cost assumptions honestly.
- **[7.4. Feedback](./7.4. Feedback.md)**: Attach human MLflow assessments to concrete traces and promote regressions safely.
- **[7.5. Online Evaluation](./7.5. Online Evaluation.md)**: Inspect bounded trace samples and design, but do not pretend to run, live scoring.
- **[7.6. Governance](./7.6. Governance.md)**: Connect identity, confirmation, transactions, append-only evidence, persistence, and residual risk.

The chapter checkpoint uses local or already-running lab telemetry. It does not deploy GCP or call a model unless the learner explicitly chooses that step.
