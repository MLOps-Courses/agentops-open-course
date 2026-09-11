# Local observability

The optional local stack is self-hosted and account-free: OpenTelemetry Collector receives OTLP and derives RED metrics with `spanmetrics`, Tempo stores traces, Loki stores logs, Prometheus stores metrics and evaluates the course alert rules, Alertmanager groups the fired alerts, and Grafana queries all three. Every host port is bound to loopback.

From the repository root:

```bash
mise run observability:up
```

The task verifies every endpoint and container hardening contract. If startup or readiness fails, it removes only the `agentops-observability` project containers while preserving their named volumes.

Point the agent at `OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318`, then use:

- Tempo trace API: <http://127.0.0.1:3200>
- Grafana dashboard: <http://127.0.0.1:3002/d/agentops-overview>
- Prometheus: <http://127.0.0.1:9090>
- Alertmanager: <http://127.0.0.1:9093>
- Loki logs API: <http://127.0.0.1:3100>

Those host ports are the documented inventory, but each one is a `${NAME:-default}` interpolation in `compose.yaml`, so a collision is survivable. Set `TEMPO_PORT`, `LOKI_PORT`, `OTEL_GRPC_PORT`, `OTEL_HTTP_PORT`, `PROMETHEUS_PORT`, `ALERTMANAGER_PORT`, `GRAFANA_PORT`, or `AGENTOPS_GATEWAY_NETWORK` in the repository-root `.env` — the `observability:up` and `observability:down` tasks load it into their environment, which is what Compose interpolates from and what the readiness scripts read. `GRAFANA_PORT` is the asymmetric one: it names the host port, while the container always listens on `3000`. Overriding a port does not rewrite the URLs printed above or in Chapter 7.

Traces and logs are linked in both directions: Grafana's Tempo datasource carries a `tracesToLogsV2` link into Loki, and Loki's `trace_id` derived field links back into Tempo. Read traces in Grafana's Explore view rather than through the raw Tempo API.

Prometheus loads `prometheus-rules.yml` (SLO burn rate, latency, collector health, token/guardrail/schema signals) and routes fired alerts to Alertmanager. The `alertmanager.yml` webhook receiver is a placeholder: on Docker Desktop it can reach a loopback receiver, while native Linux needs a receiver on the Docker bridge or in the shared network. Replace it with a real notification bridge or read alerts from the UI/API.

When agentgateway runs in Kubernetes, forward its internal metrics listener only for direct diagnosis:

```bash
kubectl -n agentops port-forward svc/agentgateway 15020:15020
```

The host Compose Prometheus job targets the host gateway container, not this forward. In-cluster, the collector scrapes `agentgateway:15020` directly and needs no port-forward.

Stop the stack while preserving data:

```bash
mise run observability:down
```

The task preserves the local Tempo, Loki, Prometheus, and Grafana volumes. Use the underlying Compose `down -v` only when you intentionally want to delete them.
