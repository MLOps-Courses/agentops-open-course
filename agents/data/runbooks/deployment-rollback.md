# Runbook: Deployment Rollback

**Applies to:** any incident that correlates with a recent deploy, where reverting to the previous known-good version is the fastest path to recovery.

## Symptoms

- Latency, errors, or a crash loop that started right after a release.
- The regression window matches a deploy timestamp.

## Diagnosis

1. Confirm the **deploy time** lines up with the start of the incident.
2. Identify the previous known-good version or image tag.
3. Check whether the release included a **database migration** (rollback may need care).

## Remediation

- Roll back the deployment to the previous known-good version.
- If a migration is involved, roll back the schema only if it is backward-compatible; otherwise fix forward.
- Verify recovery: latency/error rate should return to baseline within minutes.
- Open a follow-up to add a test or guardrail that would have caught the regression.

## Related

- `high-latency`, `elevated-errors`, `service-down` — the symptoms a rollback resolves.
