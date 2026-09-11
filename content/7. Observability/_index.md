---
title: "7. Observability"
description: "Gain insight into the agent in production: reproducibility, tracing, monitoring, alerting, cost, feedback, online evaluation, governance, and incident response."
slug: "7-observability"
aliases:
  - "/7. Observability/index.html"
---

{{% admonition abstract "In one glance" %}}

- **You will:** Bring up the telemetry stack the rest of the chapter reads from, and learn which page owns which signal.
- **You need:** `mise run install` and `mise run install:platform` done, plus Docker running. No model, cluster, or account for this page.
- **Time:** about 20 minutes the first time, because six container images have to download; about 6 once they are cached. Kind: orientation. {{% /admonition %}}

## Why a running agent needs its own telemetry plane

A **telemetry plane** is the infrastructure a running system reports itself through. That is one collector receiving traces, metrics, and logs; three stores that keep them; and one Grafana over all three. Chapters 5 and 6 made the agent reachable and kept it running; neither made its turns answerable. A finished paragraph does not say how long the turn took and where the time went, what tokens it spent, or which principal approved the write it triggered. Without those, a regression stays an anecdote and a bill stays a surprise.

In the reference agent, a turn about `INC-002` ends in a restart recommendation answering none of those three questions. Two commands here bring up six services and prove a span crossed the agent path into storage. Ten pages follow: the first pins what any measurement is attributable to, each page after it owns one signal, and all read from that one Docker Compose stack.

## Start the telemetry stack this chapter reads from

Two commands, from the repository root, in this order. The first only probes, so a missing container engine fails here, not part-way through six image pulls. The second brings up Tempo, Loki, the OpenTelemetry Collector, Prometheus, Alertmanager, and Grafana, then refuses to return until each one answers:

```bash
mise run doctor:gateway    # Docker, Compose, and the other container prerequisites
mise run observability:up  # the six services, brought up and readiness-checked
```

The tail of a successful run, in full, from the first cached start on this machine:

```text
Tempo ready: http://127.0.0.1:3200/ready
Loki ready: http://127.0.0.1:3100/ready
Prometheus ready: http://127.0.0.1:9090/-/ready
Alertmanager ready: http://127.0.0.1:9093/-/ready
Grafana ready: http://127.0.0.1:3002/api/health
OpenTelemetry Collector ready: OTLP/HTTP accepted an empty valid batch
trace path verified: agent -> collector -> Tempo (19dd7b0b800344905610d6ca043dc69e)
agentops-observability-alertmanager-1 hardened: non-root, read-only, no-new-privileges, bounded
agentops-observability-grafana-1 hardened: non-root, read-only, no-new-privileges, bounded
agentops-observability-loki-1 hardened: non-root, read-only, no-new-privileges, bounded
agentops-observability-otel-collector-1 hardened: non-root, read-only, no-new-privileges, bounded
agentops-observability-prometheus-1 hardened: non-root, read-only, no-new-privileges, bounded
agentops-observability-tempo-1 hardened: non-root, read-only, no-new-privileges, bounded
observability exposure remains loopback-only
```

Six readiness probes, then three claims that make this more than "the containers started." The `trace path verified` line pushed a synthetic span through the real agent path and read it back out of Tempo by that exact id, so a collector that accepts telemetry and quietly drops it fails startup rather than misleading you later. The six `hardened` lines — one per container, alphabetical — assert non-root, read-only root filesystem, dropped capabilities, and bounded memory and process counts, so a telemetry container cannot rewrite itself or starve the host. The last line proves every published port is bound to loopback, so nothing else on your network can reach the stack.

Open Grafana at `http://localhost:3002` and leave that tab open for the rest of the chapter. It asks for no login, and Prometheus, Loki, and Tempo are already provisioned as datasources.

**You just stood up all of it from open source, on your own machine — no SaaS account, no vendor agent.**

ADK traces stay off until [7.1. Tracing]({{< relref "/7. Observability/7.1. Tracing.md" >}}) has you accept their content risk on purpose, so an empty trace view before that page is the shipped privacy default, not a broken exporter.

## Which signal to read for each operational question

Traces, metrics, logs, assessments, and audit rows each answer a different operational question. Three shorthands arrive before the pages that own them: **RED** is rate, errors, and duration; **SLO burn** is the pace at which a service level objective spends its allowed failures; **sanitized** means personal data and credential patterns stripped, strings capped. Open the page that owns the one you need:

| When you ask...                            | Signal to read                            | Where it lives                      | Page                                                                                                     |
| ------------------------------------------ | ----------------------------------------- | ----------------------------------- | -------------------------------------------------------------------------------------------------------- |
| Can I rebuild this exact release?          | code/image/model/instruction/data lineage | Git, registry, sanitized artifacts  | [7.0. Reproducibility]({{< relref "/7. Observability/7.0. Reproducibility.md" >}}) _(hands-on)_          |
| What happened inside one turn?             | risk-accepted ADK or gateway trace        | Tempo `:3200`, read in Grafana      | [7.1. Tracing]({{< relref "/7. Observability/7.1. Tracing.md" >}}) _(hands-on)_                          |
| Is the service healthy right now?          | RED metrics, sanitized logs, dashboards   | Prometheus `:9090`, Grafana `:3002` | [7.2. Monitoring]({{< relref "/7. Observability/7.2. Monitoring.md" >}}) _(hands-on)_                    |
| When should this wake someone up?          | SLO burn, shipped alert rules             | Prometheus rules, Alertmanager      | [7.2b. Alerting]({{< relref "/7. Observability/7.2b. Alerting.md" >}}) _(hands-on)_                      |
| What did the work cost?                    | token counters plus stated assumptions    | Prometheus, trace attributes        | [7.3. Costs]({{< relref "/7. Observability/7.3. Costs.md" >}}) _(hands-on)_                              |
| What stops a caller spending the budget?   | gateway token bucket and cost ledger      | Prometheus, agentgateway rules      | [7.3b. Cost Governance]({{< relref "/7. Observability/7.3b. Cost Governance.md" >}}) _(hands-on)_        |
| Was this answer any good?                  | sanitized deterministic or judge verdict  | JSON artifact and optional OTLP     | [7.4. Feedback]({{< relref "/7. Observability/7.4. Feedback.md" >}}) _(hands-on)_                        |
| Are answers drifting at scale?             | sampled trace scoring (design only)       | a Tempo sample                      | [7.5. Online Evaluation]({{< relref "/7. Observability/7.5. Online Evaluation.md" >}}) _(concept)_       |
| Who approved this write, and what changed? | append-only audit row                     | SQLite audit table                  | [7.6. Governance]({{< relref "/7. Observability/7.6. Governance.md" >}}) _(hands-on, needs the cluster)_ |
| The agent itself broke — now what?         | detect → triage → mitigate → review       | every signal above, joined          | [7.7. Incident Response]({{< relref "/7. Observability/7.7. Incident Response.md" >}}) _(hands-on)_      |

Each page also says where the shipped stack stops: no fake dollar panel, no automatic live judge, no external paging, no cryptographically immutable audit store.

## Where each signal physically lives

**OTLP**, the OpenTelemetry wire protocol, carries traces, metrics, and logs off the agent ([0.8. Glossary]({{< relref "/0. Overview/0.8. Glossary.md#otlp" >}})). One collector receives that single stream and fans it out to three stores; Prometheus scrapes the collector and feeds both the dashboard and the alerts. Two more names matter later. **`span_metrics`** is the collector connector that derives request-count and duration metrics from spans, so the application never emits them itself. **`trace_id`** is the identifier a log record carries when it was written inside a recorded span, which lets Grafana jump from a span to its logs.

```mermaid
flowchart LR
    Agent[agentops-agent] -->|metrics and sanitized logs; ADK traces opt-in| Collector[OTel Collector]
    Gateway[agentgateway] -.->|OTLP traces :4317, k8s only| Collector
    Collector -->|traces, OTLP/HTTP :4318| Tempo[Tempo :3200]
    Collector -->|logs /otlp| Loki[Loki :3100]
    Collector -->|span_metrics :8889| Prometheus[Prometheus :9090]
    Tempo --> Grafana[Grafana :3002]
    Prometheus --> Grafana
    Loki --> Grafana
    Tempo -. "same trace_id" .-> Loki
    Loki -. "same trace_id" .-> Tempo
    Prometheus --> Alertmanager[Alertmanager :9093]
```

**Diagram in words:** The agent pushes metrics and sanitized logs to the collector on `:4318`; ADK spans use that path only after explicit risk acceptance. Kubernetes gateway traces enter on `:4317`. The collector routes available spans to Tempo, logs to Loki, and metrics to Prometheus. Grafana reads all three, and Prometheus feeds Alertmanager.

Every later page assumes you know which port belongs to which piece:

| Component                  | Port                       | What it is for                                                                                  |
| -------------------------- | -------------------------- | ----------------------------------------------------------------------------------------------- |
| OpenTelemetry Collector    | `:4317` gRPC, `:4318` HTTP | receives gateway traces, agent metrics/logs, and explicitly risk-accepted ADK traces            |
| Tempo                      | `:4318` in, `:3200` query  | stores the traces and answers the queries Grafana reads one turn through                        |
| Loki                       | `:3100/otlp`               | stores the logs                                                                                 |
| Collector metrics endpoint | `:8889`                    | exposes `span_metrics` and native metrics for Prometheus                                        |
| Prometheus                 | `:9090`                    | stores those metrics and evaluates the alert rules                                              |
| Alertmanager               | `:9093`                    | receives the alerts Prometheus fires                                                            |
| Grafana                    | `:3002`                    | dashboards and Explore over Prometheus, Loki, and Tempo, host profile only                      |
| agentgateway metrics       | `:15020`                   | the gateway's own metrics, scraped by Prometheus on the host and by the collector in Kubernetes |

Which scraper pulls those metrics, and whether you get a Grafana, depends on the deployment profile:

{{% collapsible note "Deeper: what each deployment profile ships" %}}

This table is the canonical deployment-profile split; sibling pages link back instead of restating it. Both collector profiles expose span-derived metrics at `:8889`. The Kubernetes collector also scrapes agentgateway `:15020`, so its `:8889` endpoint includes gateway metrics.

| Profile           | Config                             | Scraper + alert rules                                                        | Grafana      | How you reach it                                |
| ----------------- | ---------------------------------- | ---------------------------------------------------------------------------- | ------------ | ----------------------------------------------- |
| Host Compose      | `infra/observability/compose.yaml` | Prometheus scrapes collector `:8889`, Tempo `:3200`, gateway `:15020`        | yes, `:3002` | `localhost` ports                               |
| Local k8s overlay | `infra/k8s/overlays/local`         | own Prometheus + Alertmanager, same rules, scrape only `otel-collector:8889` | no           | `kubectl port-forward`                          |
| GKE overlay       | `infra/k8s/overlays/gke`           | none shipped; `:8889` stays a ClusterIP                                      | no           | point your own scraper at `otel-collector:8889` |

{{% /collapsible %}}

## What this chapter proved

Only the first three are true when you finish this page. The fourth lands at the end of [7.7. Incident Response]({{< relref "/7. Observability/7.7. Incident Response.md" >}}).

- `mise run observability:up` finished green, which means a real span crossed the agent path into Tempo and came back out by id.
- Grafana at `http://localhost:3002` opens without a login and lists Prometheus, Loki, and Tempo as datasources.
- For a trace, a metric, a log line, an assessment, and an audit row, you can name the page that owns it — and for the three that live behind a port, the port as well. The other two are files.
- Given a symptom, you can walk metric → trace → log → audit row without stopping to ask which tool holds which half of the answer.

The evidence this chapter produces — traces, metrics, sanitized logs, audit rows — is what [8.7. Capstone]({{< relref "/8. Community/8.7. Capstone.md" >}}) asks you to reproduce on a domain of your own.

Continue to [7.0. Reproducibility]({{< relref "/7. Observability/7.0. Reproducibility.md" >}}) once the stack is up: a signal is worth reading only if you can name the code, image, model, instruction, and data behind it.
