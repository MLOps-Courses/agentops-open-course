# AGENTS.md

Guidance for coding agents working in the AgentOps Open Course. Humans should start with [README.md](./README.md). This repository dogfoods the [AGENTS.md](https://agents.md/) convention taught in Chapter 1.

## Repository purpose

The course teaches the complete lifecycle of one **Ops Copilot** with Google ADK, agentgateway, kagent, MLflow, and OpenTelemetry. It must remain an executable course, not a collection of illustrative snippets.

- `docs/` contains FAQ-based course pages published by Zensical.
- `agents/python/` is the locked Python reference agent, offline tests, and model-backed evaluations.
- `agents/data/` is immutable seed input: SQLite, logs, runbooks, and Agent Skills.
- `clients/web/` is a minimal, offline, dependency-free A2A web client for the Ops Copilot.
- `load/` holds k6 load tests and the documented latency budgets for the platform.
- `infra/agentgateway/{host,k3d,gke}/` contains the three data-plane profiles.
- `infra/k8s/base` plus `infra/k8s/overlays/{local,gke}` contains the shared Kubernetes deployment.
- `infra/kagent/` declares the BYO Agent, gateway ModelConfig, and governed RemoteMCPServer.
- `infra/mlflow/` builds the locked non-root MLflow server.
- `infra/observability/` contains host Compose and in-cluster OTel/Prometheus/Grafana resources.
- `infra/gcp/` is a plan-first OpenTofu module for the optional GKE lab.

## Course invariants

- **Docs mirror source.** Runnable Python snippets match `agents/python`; commands/manifests match `infra`. Prefer a short exact excerpt plus a source link over a second pseudo-implementation.
- **Every course page is an FAQ.** It starts with YAML `description` front matter, contains at least one H2, and every H2 ends in `?`. `scripts/check-docs.sh` enforces this.
- **Seed and state stay separate.** `agents/data/incidents.db` is never mutated. Host writes go to `agents/python/.state`; Kubernetes agent/MCP processes share `agentops-agent-state` so reads remain coherent with approved writes.
- **Reads and writes have different authority.** Six read/runbook tools can be direct locally or MCP through `AGENT_MCP_URL`. `restart_service` and `resolve_incident` remain in-process, require ADK confirmation, validate targets, and append audit evidence in the same transaction.
- **Audit is append-only, not immutable.** SQLite triggers block row update/delete through the schema; administrators can still alter the file/schema. Do not overclaim.
- **Telemetry content stays private by default.** Both ADK/GenAI content-capture variables default to literal `false`. PII callbacks cover outbound model requests, inbound model responses, and tool output, but raw session ingestion occurs earlier.
- **No LiteLLM or garak contract.** Runtime/evaluation uses native ADK or ADK `OpenAILlm` through agentgateway. `mise run redteam` is deterministic offline adversarial regression, not live-model penetration testing.
- **Cost-efficient by default.** Prefer deterministic offline tests and fakes, the smallest model that can validate the behavior, and single-replica resource-bounded local services. Measure before increasing model size, context, RAM, CPU, storage, replicas, or load-test concurrency. Do not start a cluster, observability stack, model server, paid API, or cloud resource unless it materially validates the current boundary; stop temporary processes and tear down disposable resources when the check is complete.

## Open-source boundary

The software implementation is OSS: ADK, agentgateway, kagent, MLflow, OpenTelemetry, Prometheus, Grafana, Ollama, Qwen3 weights, and repository code. Gemini, Vertex AI, GKE, GCS, Artifact Registry, and GitHub hosting are optional proprietary services. Never call the complete environment "100% OSS" without that distinction.

Local Qwen3/Ollama is the account-free model path. Chapters 2-4 also teach native Gemini because it is ADK's direct integration. The GKE overlay uses Vertex through Workload Identity Federation; it mounts no cloud key.

## Pinned contracts

Use the repository files/locks as version authority. Current coordinated pins include:

- Google ADK Python compatible range starts at `2.4.0`; `uv.lock` is exact.
- agentgateway binary/image `1.3.1`, with the image digest pinned in Kubernetes.
- stable kagent Helm charts `0.9.11`; API resources are `v1alpha2`.
- MLflow `3.14.0` in the locked server/evaluation environments.
- OpenTelemetry Collector contrib `0.156.0` by image digest.
- Python `3.13`; Zensical is exactly pinned in the root project.

The stable network contract is MCP `:3000`, A2A `:3001`, OpenAI-compatible model `:4000`, gateway metrics `:15020`, raw MCP `:8000`, raw A2A `:8080`, MLflow `:5000`, OTLP `:4317/:4318`, Prometheus `:9090`, and host Grafana `:3002`.

## Development commands

Root tasks:

```bash
mise install
mise run install
mise run doctor
mise run format
mise run check
mise run test
mise run scan
mise run build
mise run serve
mise run gateway:host
mise run observability:up
mise run observability:down
mise run cluster:start
mise run platform:install
mise run platform:dev
```

Agent tasks from `agents/python/`:

```bash
mise run check
mise run test
mise run redteam
mise run mcp
mise run mcp:http
mise run a2a
mise run data:reset
```

`mise run eval` and `mise run eval:mlflow` call a configured model and stay outside the offline test gate. The MLflow judge is optional and must use the configured agentgateway URL.

## Local and cloud safety

The host gateway is `infra/agentgateway/host/config.yaml`. Local Kubernetes is created only from `infra/k3d.yaml`, uses `registry.localhost:5050`, and is deployed from `infra/` with:

```bash
SKAFFOLD_DEFAULT_REPO=registry.localhost:5050 skaffold dev -p local
```

Do not start host Compose observability while the in-cluster stack is forwarded on the same ports. No profile creates an Ingress, LoadBalancer, or public application endpoint; clients use temporary port-forwards through agentgateway.

The GKE path stops at `tofu plan` unless the user explicitly approves deployment. The single zonal Spot-node design is production-shaped but interruptible and non-HA. The under-USD-20 target depends on the billing account's GKE free-tier credit and light usage; it is not a guarantee. `skaffold delete`, PVC deletion, `k3d cluster delete`, `tofu apply`, and `tofu destroy` require careful context/review; cloud apply/destroy requires explicit approval.

## Documentation workflow

- Keep prose practical and question-led; finish technical pages with verification and, where relevant, teardown.
- Use only `1.` for ordered Markdown list items.
- Never add machine-specific paths, credentials, floating image tags, stale registry names, or commands that depend on private dotfiles.
- Distinguish offline tests, local model calls, hosted model calls, Kubernetes changes, and cloud changes before asking a learner to run anything.
- Do not claim alerts, feedback endpoints, online scorers, public auth/TLS, HA, backups, or cost metrics unless the repository implements and validates them.
- Update `README.md`, public component READMEs, course prose, and this file together when a public contract changes.

## Definition of done

Re-read the original request, inspect the final diff, and run:

```bash
mise run format
mise run check
mise run test
```

The Python suite enforces at least 95% branch coverage. For infrastructure changes, render/validate both overlays and run repository security checks. Never suppress a real failure to force green. Do not call a live model, deploy Kubernetes/cloud resources, or commit unless the user explicitly asks.
