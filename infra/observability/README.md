# Local observability

The optional local stack is self-hosted and account-free: MLflow 3.14 stores traces and artifacts, OpenTelemetry Collector receives OTLP and derives RED metrics with `spanmetrics`, Prometheus stores metrics and evaluates the course alert rules, Alertmanager groups the fired alerts, Loki stores logs, and Grafana queries both. Every host port is bound to loopback.

From `infra/`:

```bash
docker compose -f observability/compose.yaml up --build -d
```

Point the agent at `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318`, then use:

- MLflow traces: <http://localhost:5000>
- Grafana dashboard: <http://localhost:3002/d/agentops-overview>
- Prometheus: <http://localhost:9090>
- Alertmanager: <http://localhost:9093>
- Loki logs API: <http://localhost:3100>

Prometheus loads `prometheus-rules.yml` (SLO burn rate, latency, collector health, token/guardrail/schema signals) and routes fired alerts to Alertmanager. The `alertmanager.yml` webhook receiver points at a placeholder host endpoint: replace it with a real notification bridge or read alerts from the UI/API.

When agentgateway runs in Kubernetes, forward its internal metrics listener so Compose Prometheus can scrape the same policy traffic:

```bash
kubectl -n agentops port-forward svc/agentgateway 15020:15020
```

The in-cluster collector scrapes `agentgateway:15020` directly and needs no port-forward.

Stop the stack while preserving data:

```bash
docker compose -f observability/compose.yaml down
```

Adding `-v` intentionally deletes the local MLflow, Prometheus, Loki, and Grafana data.
