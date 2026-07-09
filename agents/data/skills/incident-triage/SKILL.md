---
name: incident-triage
description: How to prioritize and triage open incidents for the Ops Copilot.
---

# Incident Triage Skill

Use this skill when asked to triage, prioritize, or decide what to work on first.

## Instructions

1. List every incident with `list_incidents` (no `status` filter), then keep only those whose status is `open` or `investigating` (drop any `resolved`).
1. Rank them by severity — **SEV1** (highest) before SEV2 before SEV3. Break ties by the oldest `opened_at` first (longer-running incidents are more urgent).
1. For the top incident, check the affected service with `get_service_status`; a `down` service outranks a `degraded` one.
1. Report the single most urgent incident first: its id, service, severity, and one-line summary, then briefly list the rest in priority order.
1. Never invent incidents or severities — only report what the tools return.
