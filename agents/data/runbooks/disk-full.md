# Runbook: Disk Full

**Applies to:** a node or volume that has run out of disk space, causing writes to fail and services on that node to degrade or crash.

## Symptoms

- "No space left on device" errors in logs.
- Writes, log rotation, or checkpoints failing.
- Services co-located on the affected node degrading together.

## Diagnosis

1. Identify the full volume and the largest consumers (logs, temp files, cache).
2. Check whether **log rotation** is configured and working.
3. Look for runaway log growth from a noisy error loop.
4. Confirm whether the volume can be **expanded** or needs cleanup.

## Remediation

- Rotate and compress or delete old logs to reclaim space immediately.
- Expand the volume if growth is legitimate.
- Fix or throttle the log source causing runaway growth.
- Add a disk-usage alert below 100% so this is caught earlier next time.

## Related

- `service-down` — when the full disk crashed the service.
- `high-latency` — when disk pressure only slows the service.
