# Infrastructure

Run the AgentOps agent locally, then on Kubernetes. Most of this is covered in Chapters 5–6, plus the optional Chapter 7 observability stack. Licensed under the [MIT License](./LICENSE).

## Layout

- `agentgateway/config.yaml` — [agentgateway](https://agentgateway.dev) (AAIF), run standalone from this dir: `agentgateway -f agentgateway/config.yaml` (admin UI on `:15000`). Fronts the Ops Copilot MCP server; Chapter 5.3–5.5 extras are commented by chapter. See **Chapter 5**.
- `kagent/` — [kagent](https://kagent.dev) (CNCF) custom resources: `ModelConfig`, `Agent` (`type: BYO`), and a `RemoteMCPServer` (in `toolserver.yaml`) that registers the gateway's MCP endpoint. See **Chapter 6.3–6.4**.
- `k8s/` — cluster `Namespace` and `*.localhost` `Ingress`. kagent provisions the agent's Deployment and Service (both named `agentops-agent`, A2A on port 8080) from the `Agent` CR, so they are not duplicated here.
- `helmfile.yaml` — install kagent via Helm (alternative to `kagent install --profile demo`).
- `skaffold.yaml` — local build→deploy inner loop (Python image → k3d registry → apply manifests). The image is built from [`../agents/python/Dockerfile`](../agents/python/Dockerfile) (slim, non-root, serving A2A on `:8080`), with build context `../agents` so the shared dataset is included.
- `observability/` — an optional, 100% open-source [OpenTelemetry Collector → Jaeger/Prometheus/Grafana](./observability/README.md) stack (`compose.yaml`) for **Chapter 7**: `docker compose -f observability/compose.yaml up -d`, then point the agent at `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318`.

## Quickstart (Chapter 6)

```bash
dot cluster start                                   # shared local k3d cluster (k8s-local standard)
kubectl apply -f k8s/namespace.yaml
kubectl -n agentops create secret generic kagent-gemini --from-literal=GOOGLE_API_KEY="$GOOGLE_API_KEY"
helmfile apply                                      # or: kagent install --profile demo
skaffold run                                        # build (Docker) + deploy the agent
```

Prefer to build and deploy by hand (without skaffold)? The build context is `../agents`, so the shared dataset is included:

```bash
docker build -f ../agents/python/Dockerfile -t k3d-registry.localhost:5050/agentops-agent:latest ../agents
docker push k3d-registry.localhost:5050/agentops-agent:latest
kubectl apply -f kagent/modelconfig.yaml -f kagent/agent.yaml -f k8s/ingress.yaml
```

> Versions are pinned (kagent `v0.10.0-beta3`, agentgateway `v1.3.1`) — **re-verify at build time**; these projects move fast and APIs are still `v1alpha*`.
