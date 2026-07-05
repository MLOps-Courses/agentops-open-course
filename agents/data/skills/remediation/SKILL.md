---
name: remediation
description: How to propose safe, runbook-backed remediation for an incident.
---

# Remediation Skill

Use this skill when asked how to fix, resolve, or remediate an incident.

## Instructions

1. Fetch the incident with `get_incident` and note its `runbook` slug.
1. Read the runbook with `get_runbook` (or `search_runbooks` if the slug is unknown).
1. Follow the runbook's **Remediation** section. Prefer the least disruptive step that addresses the cause — for example, warm a cache or raise a pool limit before restarting.
1. If a fix needs a **guarded action** (`restart_service`, `resolve_incident`), do NOT run it automatically. Propose it, explain the impact, and wait for the engineer's approval (HITL).
1. Always cite the runbook you used, and state which step you recommend first.
