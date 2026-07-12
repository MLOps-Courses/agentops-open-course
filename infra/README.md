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

The pre-Kubernetes profile expects host processes on MCP `:8000`, A2A `:8080`, Ollama `:11434`, and OTLP/gRPC `:4317`:

```bash
agentgateway --validate-only -f agentgateway/host/config.yaml
agentgateway -f agentgateway/host/config.yaml
```

The `host`, `k3d`, and `gke` files carry the same policies; only their upstream endpoints and model provider differ, and the Kubernetes profiles enforce the demo API key on the model listener. The secured profile must start from the repository root so its relative auth-material paths resolve:

```bash
infra/scripts/gateway-tls.sh
infra/scripts/gateway-jwt.sh >/dev/null
agentgateway -f infra/agentgateway/host/config-auth.yaml
```

## Local Kubernetes

Prerequisites are Docker, k3d, kubectl, Helm, Helmfile, Skaffold, Kustomize, and Ollama. From `infra/`:

```bash
k3d cluster create --config k3d.yaml
kubectl apply -f k8s/base/namespace.yaml
helmfile apply
```

Bind Ollama only to the k3d bridge, then pull the Apache-2.0 Qwen model in a second shell with the same `OLLAMA_HOST` value:

```bash
export OLLAMA_HOST="$(docker network inspect k3d-local --format '{{(index .IPAM.Config 0).Gateway}}'):11434"
ollama serve
```

```bash
export OLLAMA_HOST="$(docker network inspect k3d-local --format '{{(index .IPAM.Config 0).Gateway}}'):11434"
ollama pull qwen3:4b
SKAFFOLD_DEFAULT_REPO=registry.localhost:5050 skaffold dev -p local
```

No Ingress or LoadBalancer is created. Open only the path being tested:

```bash
kubectl -n agentops port-forward svc/agentops-agent 8080:8080
kubectl -n agentops port-forward svc/agentgateway 3000:3000 3001:3001 4000:4000 15020:15020
kubectl -n agentops port-forward svc/mlflow 5000:5000
```

The local overlay sends the agent through agentgateway to `qwen3:4b`; it does not need a provider key. `OPENAI_API_KEY=agentgateway` is a non-secret marker that the gateway's model listener now enforces as a demo API key. The direct `agentops-mcp:8000` Service is reachable only behind the gateway.

The agent and MCP server share one RWO `agentops-agent-state` claim so SQLite reads and guarded writes stay coherent. The claim constrains both consumers to a compatible node. This is a single-replica course architecture, not horizontally scalable SQLite.

## Host observability

Use the Compose stack when running the agent directly on the host, not at the same time as the in-cluster MLflow/collector on the same ports:

```bash
docker compose -f observability/compose.yaml up --build -d
```

Set `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318`. MLflow is at <http://localhost:5000>, the provisioned Grafana dashboard at <http://localhost:3002/d/agentops-overview>, Prometheus at <http://localhost:9090>, and Alertmanager at <http://localhost:9093>. See `observability/README.md` for gateway metrics and the shipped alert rules. In the local Kubernetes overlay, the same rules run in an in-cluster Prometheus/Alertmanager pair reachable via `kubectl -n agentops port-forward`.

## GKE

The OpenTofu defaults use project `agentops-open-course`, one zonal Spot `e2-standard-2` node, public node IPs instead of a chargeable NAT, and no public application endpoint. Review `gcp/README.md`, authenticate ADC, and plan first:

```bash
cd gcp
tofu init
tofu validate
tofu plan -out=tfplan
```

After a separately approved apply, retrieve credentials using the command in `tofu output -raw get_credentials_command`. Then return to `infra/`:

```bash
kubectl apply -f k8s/base/namespace.yaml
helmfile apply
SKAFFOLD_DEFAULT_REPO="$(cd gcp && tofu output -raw artifact_registry_repository)" skaffold run -p gke
```

GKE agentgateway obtains a Vertex access token from ambient Workload Identity; MLflow uses its own identity for GCS. Neither workload has a static cloud key.

## Teardown

`skaffold delete -p local` or `skaffold delete -p gke` also deletes the course PVCs and their data. `docker compose -f observability/compose.yaml down` preserves named volumes; adding `-v` deletes them. The `local` cluster and kagent control plane can be shared by other projects, so `helmfile destroy` and `k3d cluster delete local` are dedicated-lab operations, not routine course cleanup. GCP destruction is likewise separate and must be confirmed from a reviewed plan.
