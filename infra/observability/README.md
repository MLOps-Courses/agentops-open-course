# Local observability

The optional local stack is self-hosted and account-free: MLflow 3.14 stores traces and artifacts, OpenTelemetry Collector receives OTLP and derives RED metrics with `spanmetrics`, Prometheus stores metrics and evaluates the course alert rules, Alertmanager groups the fired alerts, Loki stores logs, and Grafana queries both. Every host port is bound to loopback.

From the repository root:

```bash
mise run observability:up
```

Point the agent at `OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318`, then use:

- MLflow traces: <http://127.0.0.1:5000>
- Grafana dashboard: <http://127.0.0.1:3002/d/agentops-overview>
- Prometheus: <http://127.0.0.1:9090>
- Alertmanager: <http://127.0.0.1:9093>
- Loki logs API: <http://127.0.0.1:3100>

Prometheus loads `prometheus-rules.yml` (SLO burn rate, latency, collector health, token/guardrail/schema signals) and routes fired alerts to Alertmanager. The `alertmanager.yml` webhook receiver points at a placeholder host endpoint: replace it with a real notification bridge or read alerts from the UI/API.

When agentgateway runs in Kubernetes, forward its internal metrics listener so Compose Prometheus can scrape the same policy traffic:

```bash
kubectl -n agentops port-forward svc/agentgateway 15020:15020
```

The in-cluster collector scrapes `agentgateway:15020` directly and needs no port-forward.

Stop the stack while preserving data:

```bash
mise run observability:down
```

The task preserves the local MLflow, Prometheus, Loki, and Grafana volumes. Use the underlying Compose `down -v` only when you intentionally want to delete them.
