# Infrastructure

The same container images run on a local k3d cluster and a small GKE Standard cluster. The software data plane is OSS: Google ADK, agentgateway, kagent, MLflow, OpenTelemetry, Prometheus, and Grafana. GKE, Vertex AI, Artifact Registry, and GCS are optional managed Google Cloud services, not OSS.

## Layout

- `agentgateway/{host,k3d,gke}/config.yaml` declares separate MCP `:3000`, A2A `:3001`, and OpenAI-compatible LLM `:4000` listeners. Metrics stay internal on `:15020`.
- `agentgateway/host/config-auth.yaml` is the opt-in secured host profile: strict JWT on MCP/A2A, an enforced API key on the model route, and TLS on all three listeners, backed by demo material from `scripts/gateway-{tls,jwt}.sh` (gitignored under `agentgateway/host/auth/`).
- `k8s/base` and `k8s/overlays/{local,gke}` are the Kustomize deployment.
- `k8s/base/secrets/` holds SOPS-encrypted Secret manifests (age recipient in the root `.sops.yaml`). `scripts/secrets.sh` generates the gitignored age key under `infra/secrets/`, then encrypts, decrypts, or edits manifests; deploy one with `scripts/secrets.sh decrypt <file> | kubectl apply -f -`. Encrypted files stay out of the Kustomize overlays so rendering never needs the private key.
- `kagent` contains the BYO `Agent`, gateway `ModelConfig`, MCP registration, and a slim stable-chart values file.
- `mlflow` builds a locked MLflow 3.14 image that runs as UID 10002.
- `observability` is the loopback-only host stack for running the agent outside Kubernetes.
- `gcp` is an OpenTofu module. It never runs kubectl or gcloud provisioners.

## Host gateway

The pre-Kubernetes profile expects host processes on MCP `:8000`, A2A `:8080`, Ollama `:11434`, and OTLP/gRPC `:4317`. From the repository root, run the digest-pinned image through the checked wrapper:

```bash
mise run doctor:gateway
mise run gateway:host
```

The wrapper publishes every gateway listener on `127.0.0.1`, drops capabilities, uses a read-only filesystem, and removes only its labelled container. On native Linux, a wrapper-owned relay listens only on Docker's bridge gateway and forwards to the loopback MCP, A2A, and Ollama processes. Docker's loopback publication makes gateway `:15020` available to Compose Prometheus without exposing raw upstream or metrics ports on the workstation's LAN interfaces. Detached lifecycle tasks are `gateway:host:start`, `gateway:host:status`, `gateway:host:logs`, and `gateway:host:stop`, including relay cleanup.

The `host`, `k3d`, and `gke` files carry the same policies; only their upstream endpoints and model provider differ, and the Kubernetes profiles enforce the demo API key on the model listener. The raw agentgateway binary currently binds configured listeners on all interfaces, so it is an advanced/manual path rather than the host quickstart.

The secured profile uses the same hardened wrapper. Its task generates demo material, stages only the listener certificate/private key and public JWKS into the wrapper's private runtime directory, and keeps the CA and JWT signing keys on the host:

```bash
mise run gateway:host:auth
```

It adds demo JWT/API-key/TLS controls while preserving loopback-only publication, the read-only container filesystem, dropped capabilities, and scoped cleanup.

## Local Kubernetes

Prerequisites are Docker, k3d, kubectl, Helm, Helmfile, Skaffold, Kustomize, and Ollama. Kubernetes begins in Chapter 6. From the repository root:

```bash
mise run doctor:platform
mise run cluster:start
mise run platform:install
```

Bind Ollama only to the k3d bridge, then pull the Apache-2.0 open-weight Qwen3 model in a second shell with the same `OLLAMA_HOST` value:

```bash
export OLLAMA_HOST="$(docker network inspect k3d-local --format '{{(index .IPAM.Config 0).Gateway}}'):11434"
ollama serve
```

```bash
export OLLAMA_HOST="$(docker network inspect k3d-local --format '{{(index .IPAM.Config 0).Gateway}}'):11434"
ollama pull qwen3:4b
cd infra
SKAFFOLD_DEFAULT_REPO=registry.localhost:5050 skaffold dev -p local
```

No Ingress or LoadBalancer is created. Open only the path being tested:

```bash
kubectl -n agentops port-forward svc/agentops-agent 8080:8080
kubectl -n agentops port-forward svc/agentgateway 3000:3000 3001:3001 4000:4000 15020:15020
kubectl -n agentops port-forward svc/mlflow 5000:5000
```

The local overlay keeps `AGENT_MODEL_PROVIDER=openai-compatible` and sends the agent through agentgateway to `qwen3:4b`; it does not need an upstream provider key. `OPENAI_API_KEY=agentgateway` is a non-secret marker that the Kubernetes gateway model listener enforces as a demo API key. The direct `agentops-mcp:8000` Service is reachable only behind the gateway.

The agent and MCP server share one RWO `agentops-agent-state` claim so SQLite reads and guarded writes stay coherent. Only the agent mounts it writable; the six-tool MCP service mounts it read-only and remains unready until the agent initializes the runtime database. The claim constrains both consumers to a compatible node. This is a single-replica course architecture, not horizontally scalable SQLite.

## Host observability

Use the Compose stack when running the agent directly on the host, not at the same time as the in-cluster MLflow/collector on the same ports:

```bash
mise run observability:up
```

Set `OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318`. MLflow is at <http://127.0.0.1:5000>, the provisioned Grafana dashboard at <http://127.0.0.1:3002/d/agentops-overview>, Prometheus at <http://127.0.0.1:9090>, and Alertmanager at <http://127.0.0.1:9093>. See `observability/README.md` for gateway metrics and the shipped alert rules. In the local Kubernetes overlay, the same rules run in an in-cluster Prometheus/Alertmanager pair reachable via `kubectl -n agentops port-forward`.

## GKE

The OpenTofu defaults use project `agentops-open-course`, one zonal Spot `e2-standard-2` node, public node IPs instead of a chargeable NAT, and no public application endpoint. Review `gcp/README.md`, authenticate ADC, run `mise run doctor:gcp`, and plan first:

```bash
cd infra/gcp
tofu init
tofu validate
tofu plan -out=tfplan
```

After a separately approved apply, retrieve credentials using the command in `tofu output -raw get_credentials_command`. Then return to `infra/`:

```bash
cd ..
kubectl apply -f k8s/base/namespace.yaml
helmfile apply
SKAFFOLD_DEFAULT_REPO="$(cd gcp && tofu output -raw artifact_registry_repository)" skaffold run -p gke
```

GKE agentgateway obtains a Vertex access token from ambient Workload Identity; MLflow uses its own identity for GCS. Neither workload has a static cloud key.

## Teardown

`skaffold delete -p local` or `skaffold delete -p gke` also deletes the course PVCs and their data. `mise run observability:down` preserves named volumes; adding Compose `-v` deletes them. Stop a detached host gateway with `mise run gateway:host:stop`. The `local` cluster and kagent control plane can be shared by other projects, so `helmfile destroy` and `k3d cluster delete local` are dedicated-lab operations, not routine course cleanup. GCP destruction is likewise separate and must be confirmed from a reviewed plan.
