---
name: incident-triage
description: Prioritize open incidents deterministically. Use when the engineer asks what to investigate first, requests a queue ranking, or needs an evidence-backed triage summary.
---

# Incident Triage Skill

## Instructions

1. List every incident with `list_incidents` (no `status` filter), then keep only those whose status is `open` or `investigating` (drop any `resolved`).
1. Stop with a clear data-quality error if an active incident has no id, service, supported severity, or `opened_at`; do not guess a ranking key.
1. Rank by severity — **SEV1** before SEV2 before SEV3 — then by the oldest `opened_at` first.
1. For tied top candidates, call `get_service_status`; a `down` service outranks a `degraded` service, which outranks a healthy service.
1. Report the most urgent incident first with its id, service, severity, age evidence, current service state, and one-line summary. List the remaining active incidents in priority order.
1. Separate observed facts from your recommendation. Do not invent incidents, severities, timestamps, or service state, and do not run remediation from a triage request.
