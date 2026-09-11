# Runbook: Memory Leak

**Applies to:** a service whose memory usage grows steadily with uptime until it is OOM-killed, evicts data, or must be restarted on a schedule that tracks the leak rate.

## Symptoms

- RSS or heap grows linearly with uptime and resets on restart (a sawtooth pattern).
- Periodic OOM kills or scheduled restarts; latency often rises as memory pressure grows.
- On cache nodes: eviction rate climbs and hit-rate falls as the leak squeezes usable memory.

## Diagnosis

1. Plot memory against **uptime** — a sawtooth that resets on restart indicates a leak, not load.
2. Compare against request volume: a leak keeps growing even under flat traffic.
3. Capture a heap profile at the high watermark and diff it against a fresh instance.
4. Correlate the leak's start with a deploy that touched buffers, caches, or serialization.

## Remediation

- Roll back or patch the leaking release (see `deployment-rollback`).
- As a stopgap, schedule rolling restarts before the OOM threshold — and treat that as debt, not a fix.
- Raise memory limits only to buy diagnosis time.
- On cache nodes, watch eviction and hit-rate: a leak there can overload dependencies (see `cascade-failure`).

## Related

- `cascade-failure` — when the leak's evictions overload upstream-dependent services.
- `deployment-rollback` — reverting the release that introduced the leak.
