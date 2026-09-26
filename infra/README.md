# Infrastructure

The same container images run on a local k3d cluster and a small GKE Standard cluster. The software data plane is OSS: Google ADK, agentgateway, kagent, MLflow, OpenTelemetry, Prometheus, and Grafana. GKE, Vertex AI, Artifact Registry, and GCS are optional managed Google Cloud services, not OSS.

## Layout

- `agentgateway/host/config-gemini.yaml` is the main host profile. `agentgateway/{host,k3d}/config.yaml` retains the optional Ollama/fake profile, and `agentgateway/gke/config.yaml` uses Vertex. All declare MCP `:3000`, A2A `:3001`, and OpenAI-compatible model `:4000` listeners, with metrics on `:15020`.
- `agentgateway/host/config-auth.yaml` is the opt-in secured host profile: strict JWT on MCP/A2A, an enforced API key on the model route, and TLS on all three listeners, backed by demo material from `scripts/gateway-{tls,jwt}.sh` (gitignored under `agentgateway/host/auth/`).
- `k8s/base` and `k8s/overlays/{local,local-gemini,gke}` are the Kustomize deployment. `local-gemini` is the learner default; `local` is the explicit Ollama/fake alternative.
- `k8s/base/secrets/` holds SOPS-encrypted Secret manifests (age recipient in the root `.sops.yaml`). `scripts/secrets.sh` generates the gitignored age key under `infra/secrets/`, then encrypts, decrypts, or edits manifests; deploy one with `scripts/secrets.sh decrypt <file> | kubectl apply -f -`. Encrypted files stay out of the Kustomize overlays so rendering never needs the private key.
- `kagent` contains the BYO `Agent`, gateway `ModelConfig`, MCP registration, and a slim stable-chart values file.
- `mlflow` builds a locked MLflow 3.15 image that runs as UID 10002.
- `observability` is the loopback-only host stack for running the agent outside Kubernetes.
- `gcp` is an OpenTofu module. It never runs kubectl or gcloud provisioners.

## Host gateway

The pre-Kubernetes profile expects MCP on `:8000`, A2A on `:8080`, and `GOOGLE_API_KEY` in the root `.env`. It calls hosted Gemini, so interactive turns consume provider quota and may be billed. From the repository root, run the digest-pinned image through the checked wrapper:

```bash
mise run doctor:gateway
mise run gateway:host
```

The wrapper publishes every gateway listener on `127.0.0.1`, drops capabilities, uses a read-only filesystem, and removes only its labelled container. On native Linux, a wrapper-owned bridge relay reaches the loopback MCP and A2A processes. `gateway:host:ollama` selects the optional local profile and additionally reaches Ollama on `:11434`. Compose joins that scoped bridge and scrapes gateway `:15020` directly through the stable `agentops-gateway` alias. Detached lifecycle tasks are `gateway:host:start`, `gateway:host:status`, `gateway:host:logs`, and `gateway:host:stop`, including relay cleanup.

[5.1. Gateway Setup](../docs/5.%20Gateway/5.1.%20Gateway%20Setup.md) owns the three-terminal startup and the application's gateway environment. Discovery and `smoke:host` do not prove live Gemini behavior; the smoke uses a fake model.

The `host`, `k3d`, and `gke` files carry the same policies; only their upstream endpoints and model provider differ, and the Kubernetes profiles enforce the demo API key on the model listener. The raw agentgateway binary currently binds configured listeners on all interfaces, so it is an advanced/manual path rather than the host quickstart.

The secured profile uses the same hardened wrapper. Its task generates demo material, stages only the listener certificate/private key and public JWKS into the wrapper's private runtime directory, and keeps the CA and JWT signing keys on the host:

```bash
mise run gateway:host:auth
```

It adds demo JWT/API-key/TLS controls while preserving loopback-only publication, the read-only container filesystem, dropped capabilities, and scoped cleanup.

## Local Kubernetes

Kubernetes begins in Chapter 6 and needs substantially more memory than the offline Python path. Use manifest rendering without starting a cluster when the machine lacks headroom. Install the platform tier first; the pinned kubectl provides the Kustomize renderer. From the repository root:

```bash
mise run doctor:platform
mise run cluster:start
mise run platform:install
mise run platform:credentials
mise run platform:dev
```

`platform:credentials` writes the root dotenv Gemini key to the course-owned `gemini-provider` Secret. `platform:dev` selects `local-gemini`; only the gateway mounts the real provider key. For local Ollama, follow the bridge-binding and cleanup instructions in [6.2. Platform Install](../docs/6.%20Platform/6.2.%20Platform%20Install.md#how-do-you-start-the-local-kubernetes-workloads), then use `platform:dev:ollama`. Do not mix that profile with the default Gemini startup.

No Ingress or LoadBalancer is created. Open only the path being tested, and run each foreground forward in its own terminal:

```bash
kubectl -n agentops port-forward svc/agentops-agent 8080:8080
```

```bash
kubectl -n agentops port-forward svc/agentgateway 3000:3000 3001:3001 4000:4000 15020:15020
```

```bash
kubectl -n agentops port-forward svc/mlflow 5000:5000
```

Both local profiles keep `AGENT_MODEL_PROVIDER=openai-compatible` and send the agent through agentgateway. `local-gemini` uses hosted Gemini; `local` uses `qwen3:4b-instruct` without an upstream provider key. `OPENAI_API_KEY=agentgateway` is a non-secret marker that the Kubernetes gateway model listener enforces as a demo API key. The direct `agentops-mcp:8000` Service is reachable only behind the gateway.

The agent and MCP server share one RWO `agentops-agent-state` claim so SQLite reads and guarded writes stay coherent. Only the agent mounts it writable; the six-tool MCP service mounts it read-only and remains unready until the agent initializes the runtime database. The claim constrains both consumers to a compatible node. This is a single-replica course architecture, not horizontally scalable SQLite.

## Host observability

Use the Compose stack when running the agent directly on the host, not at the same time as the in-cluster MLflow/collector on the same ports:

```bash
mise run observability:up
```

Set `OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318`. MLflow is at <http://127.0.0.1:5000>, the provisioned Grafana dashboard at <http://127.0.0.1:3002/d/agentops-overview>, Prometheus at <http://127.0.0.1:9090>, and Alertmanager at <http://127.0.0.1:9093>. See `observability/README.md` for gateway metrics and the shipped alert rules. In the local Kubernetes overlay, the same rules run in an in-cluster Prometheus/Alertmanager pair reachable via `kubectl -n agentops port-forward`.

## GKE

The OpenTofu module requires `project_id` and uses one zonal Spot `e2-standard-2` node, public node IPs instead of a chargeable NAT, and no public application endpoint. Review `gcp/README.md`, authenticate ADC, run `GCP_PROJECT_ID=<project-id> mise run doctor:gcp`, and plan first:

```bash
cd infra/gcp
tofu init
tofu validate
tofu plan -out=tfplan
```

After a separately approved apply, retrieve credentials using the command in `tofu output -raw get_credentials_command`. Then return to the repository root:

```bash
cd ../..
mise run gke:deploy
```

The task verifies the exact GKE context before it installs kagent, builds and pushes images, resolves the project-neutral manifest from OpenTofu outputs, validates it, and applies it. GKE agentgateway obtains a Vertex access token from ambient Workload Identity; MLflow uses its own identity for GCS. Neither workload has a static cloud key.

Prove the compatibility-pinned Vertex function-call loop and one read-only A2A retrieval. This invokes the billed model and belongs only inside an explicitly approved lab:

```bash
mise run gke:smoke
```

The task rejects the wrong context, source image, or live model configuration. It owns random loopback-only forwards for the check and closes them afterward.

## Teardown

Stop the Skaffold watcher with Ctrl-C; its `--cleanup=false` setting preserves workloads and data. For a cluster you own, `k3d cluster stop local` releases node memory between sessions. Stop a detached host gateway with `mise run gateway:host:stop`; `mise run observability:down` preserves its named volumes.

`skaffold delete` with the deployed profile deletes course PVCs and their data. Review backups and the exact context first using [6.6. Platform Delivery](../docs/6.%20Platform/6.6.%20Platform%20Delivery.md). Namespace names alone do not establish ownership. `helmfile destroy` and `k3d cluster delete local` are dedicated-lab operations; GCP destruction requires its own reviewed plan and approval.
