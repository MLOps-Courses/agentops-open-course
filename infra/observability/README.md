# Local Observability Stack

An optional, 100% open-source backend for the agent's telemetry — the runnable companion to **Chapter 7**. It stands up the "one OTLP pipeline, many backends" story from [7.1](../../docs/7.%20Observability/7.1.%20Tracing.md)–[7.3](../../docs/7.%20Observability/7.3.%20Costs.md):

```
agent --(OTLP :4318)--> otel-collector ──> Jaeger      (traces)   http://localhost:16686
                                       └──> Prometheus  (metrics)  http://localhost:9090
                                                  Grafana (dashboards)  http://localhost:3001
```

Everything is self-hosted with no accounts: the [OpenTelemetry Collector](https://opentelemetry.io/docs/collector/) (Apache-2.0), [Jaeger](https://www.jaegertracing.io/) (Apache-2.0), [Prometheus](https://prometheus.io/) (Apache-2.0), and [Grafana](https://grafana.com/oss/grafana/) (AGPLv3). Nothing here is required to build or run the agent — it is the sink the agent exports to when you want to _see_ the telemetry.

## Run it

```bash
docker compose -f infra/observability/compose.yaml up -d
```

Then point the agent at the collector (the agent reads the repo-root `.env`):

```bash
# .env
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
OTEL_SERVICE_NAME=ops-copilot
```

Run the agent (`cd agents/python && mise run web`), ask the Ops Copilot a question, then:

- **Traces** — open Jaeger at <http://localhost:16686>, pick service `ops-copilot`, and read the span tree (model calls, tool calls, workflow nodes) from [7.1](../../docs/7.%20Observability/7.1.%20Tracing.md).
- **Metrics** — open Prometheus at <http://localhost:9090> to query latency/token/tool-call series from [7.2](../../docs/7.%20Observability/7.2.%20Monitoring.md).
- **Dashboards** — open Grafana at <http://localhost:3001> (anonymous admin; Prometheus + Jaeger are pre-provisioned) and build the SLO panels from 7.2.

Tear it down with `docker compose -f infra/observability/compose.yaml down` (add `-v` to drop volumes).

## Files

- `compose.yaml` — the four services and their host ports.
- `otel-collector.yaml` — the collector pipeline (OTLP in; Jaeger + Prometheus out).
- `prometheus.yml` — scrapes the collector's Prometheus exporter.
- `grafana/datasources.yaml` — auto-provisions the Prometheus and Jaeger datasources.

> Image tags are pinned in `compose.yaml` for reproducibility; bump and re-verify them at setup time.
