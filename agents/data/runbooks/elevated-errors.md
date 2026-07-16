# Runbook: Elevated Errors

**Applies to:** a service that is up and responding but returning an abnormal rate of errors (4xx/5xx, declines, validation failures).

## Symptoms

- Error rate above the normal baseline (e.g. declines, 5xx, token failures).
- Requests complete quickly but with failing status codes.
- A subset of users or endpoints affected.

## Diagnosis

1. Break errors down by **status code and endpoint** to localize the fault.
2. Check for **expired credentials or certificates** on downstream calls.
3. Inspect **connection-pool** exhaustion and database errors.
4. Correlate with a recent deploy, feature flag, or dependency change.
5. Check upstream/third-party status pages for partner outages.

## Remediation

- Rotate expired certificates or credentials.
- Raise connection-pool size if the pool is exhausted.
- Roll back the correlated deploy or disable the offending feature flag.
- Add a retry/backoff or circuit breaker for a flaky downstream dependency.

## Related

- `high-latency` — when errors are accompanied by slowness.
- `deployment-rollback` — reverting a bad release.
