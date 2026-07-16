---
description: Make the agent correct and trustworthy: typing, linting, testing, metrics, evaluations, guardrails, and security.
---

# 4. Quality

## How will you make the agent trustworthy?

Make the agent correct and trustworthy with layers of evidence. Start with trusted types, warning-free checks, isolated state, and branch-covered tests. Add live model trajectory evaluation separately. Then enforce PII boundaries, human confirmation, transactional writes, deterministic adversarial regressions, and repository security scans.

This chapter covers:

- **[4.0. Typing](./4.0. Typing.md)**: Python typing with ty, parsing tool I/O at the boundary.
- **[4.1. Linting](./4.1. Linting.md)**: Lint and format with ruff and dprint.
- **[4.2. Testing](./4.2. Testing.md)**: Fast, offline unit tests with pytest, against an isolated dataset copy.
- **[4.3. Metrics](./4.3. Metrics.md)**: A concrete scorecard of release gates and observed operational indicators.
- **[4.4. Evaluations](./4.4. Evaluations.md)**: ADK trajectories plus full-conversation MLflow lineage and optional judge evidence.
- **[4.5. Guardrails](./4.5. Guardrails.md)**: Boundary redaction, stable errors, confirmation, transactions, and audit evidence.
- **[4.6. Security](./4.6. Security.md)**: Threat modeling, offline adversarial regressions, identity, and supply-chain scanning.

The chapter remains model-free until the evaluation page explicitly asks for a configured provider. A green interactive demo cannot substitute for these gates.
