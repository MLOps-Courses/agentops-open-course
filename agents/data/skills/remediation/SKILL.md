---
name: remediation
description: Propose and verify safe, runbook-backed incident remediation. Use when the engineer asks how to fix or resolve a known incident, or explicitly approves a guarded mock action.
---

# Remediation Skill

## Instructions

1. Fetch the incident with `get_incident`. If it is already resolved, report that state and stop.
1. Read the runbook with `get_runbook` (or `search_runbooks` if the slug is unknown).
1. Follow the runbook's **Remediation** section. Recommend the least disruptive step that addresses the diagnosed cause, and state the expected evidence of recovery plus the rollback or stop condition.
1. If the plan needs `restart_service` or `resolve_incident`, do not run it automatically. Explain the exact target and impact, then wait for explicit approval with an attributable approver and bounded rationale.
1. After an approved action, re-read the incident and affected service. Report the resulting state and audit evidence; never claim success from the action response alone.
1. Cite the runbook, distinguish observation from recommendation, and state plainly when evidence is insufficient or conflicts with the procedure.
