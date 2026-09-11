---
name: remediation
description: Propose and verify safe, runbook-backed incident remediation. Use when the engineer asks how to fix a known incident or initiate or approve a guarded mock action.
---

# Remediation Skill

## Instructions

1. Fetch the incident with `get_incident`. If it is already resolved, report that state and stop.
1. Read the runbook with `get_runbook` (or `search_runbooks` if the slug is unknown).
1. Follow the runbook's **Remediation** section. Recommend the least disruptive step that addresses the diagnosed cause, and state the expected evidence of recovery plus the rollback or stop condition.
1. If the engineer asks to initiate `restart_service` or `resolve_incident`, explain the exact target and impact, then call the guarded tool. ADK pauses before execution and creates the confirmation request; the initiating message is not approval. Wait for an attributable approver and bounded rationale.
1. After an approved action, re-read the incident and affected service. Report the resulting state and audit evidence; never claim success from the action response alone.
1. Cite the runbook, distinguish observation from recommendation, and state plainly when evidence is insufficient or conflicts with the procedure.
