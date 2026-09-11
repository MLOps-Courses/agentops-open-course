# Runbook: Service Down

**Applies to:** a service that is fully unavailable — health checks fail and requests return 5xx or connection errors.

## Symptoms

- Health/readiness probes failing; pods crash-looping or not ready.
- Requests return HTTP 503 or connection refused.
- Availability drops toward 0%.

## Diagnosis

1. Check pod/replica status and recent restarts (`kubectl get pods`, restart counts).
2. Read the crash logs for the failing container — panics, OOM kills, or config errors.
3. Verify required **dependencies** (database, secrets, config maps) are reachable.
4. Confirm the last deploy succeeded and the image tag is correct.
5. Check resource limits — an OOM kill points to memory limits set too low.

## Remediation

- If a bad release caused it, **roll back** to the last healthy version (see `deployment-rollback`).
- **Restart** the service to clear a transient crash loop once the cause is understood.
- Fix the missing dependency or configuration, then let the deployment recover.
- Raise memory limits if the container is being OOM-killed.

## Related

- `deployment-rollback` — reverting a bad release.
- `high-latency` — partial degradation rather than a full outage.
