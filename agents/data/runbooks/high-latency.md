# Runbook: High Latency

**Applies to:** a service whose request latency (p95/p99) has risen well above its normal baseline while still returning successful responses.

## Symptoms

- p95 or p99 latency several times higher than the 7-day baseline.
- Timeouts or slow responses reported by downstream callers.
- Error rate usually normal (requests succeed, just slowly).

## Diagnosis

1. Check whether the spike lines up with a recent **deploy** or config change.
2. Inspect CPU, memory, and thread/goroutine counts for saturation.
3. Look at **downstream dependencies** (database, cache, third-party APIs) for slow calls.
4. Check cache **hit-rate**; a cold or thrashing cache is a common cause after a rebuild.
5. Review connection-pool and queue depths for contention.

## Remediation

- If a recent deploy correlates, **roll back** to the previous version (see `deployment-rollback`).
- Warm or repair the cache if hit-rate collapsed.
- Scale out replicas to shed load while you find the root cause.
- Raise connection-pool limits if the pool is the bottleneck.

## Related

- `deployment-rollback` — reverting a bad release.
- `elevated-errors` — when latency is accompanied by failures.
