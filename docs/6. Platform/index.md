---
description: Run the same private AgentOps data plane on local k3d and an optional, explicitly planned GKE lab.
---

# 6. Platform

!!! abstract "In one glance"

    - **You will:** See where each piece of the Kubernetes deployment lives, and prove both environments render before you install anything.
    - **You need:** Chapter 5 finished and `mise run doctor:platform` passing.
    - **Time:** about 12 minutes, orientation.

**Part II — Platform engineering.** This part is more demanding and assumes container and Kubernetes knowledge. Complete [4.8. Developer Handoff](../4.%20Quality/4.8.%20Developer%20Handoff.md) or begin from its tested reference checkpoint. Prepare with [Kubernetes Basics](https://kubernetes.io/docs/tutorials/kubernetes-basics/) if needed.

## Where will you run the agent?

Until now you started the agent yourself and restarted it when it died. From here the cluster does that.

Chapters 1-5 ran the reference agent as host processes behind agentgateway. This chapter moves that same validated data plane onto Kubernetes: first onto a local [k3d](https://k3d.io/) cluster driven by [kagent](https://kagent.dev/), then, optionally and without applying, onto a GKE plan. The application, protocol, and model-endpoint contracts do not change. The cluster only adds six things around them:

1. **Declarative identity**: each workload runs as a service account you declared, not as whoever launched it.
1. **Resource bounds**: CPU and memory limits, so one pod cannot starve the others.
1. **Health probes**: the cluster calls an endpoint on a schedule and acts when it stops answering.
1. **Network policy**: an explicit allowlist of which pod may reach which.
1. **Persistent state**: a volume that outlives the pod using it.
1. **Rollout ownership**: the cluster restarts and replaces pods, so you stop doing it by hand.

[6.0. Platform](./6.0. Platform.md) owns that "what changes when you move to Kubernetes" argument — read it first.

This page applies nothing and creates no cluster. [6.2. Platform Install](./6.2.%20Platform%20Install.md) creates the cluster, installs kagent, and starts the workloads before the following pages inspect them.

The install is a short, ordered path, and each step is owned by exactly one sub-page:

```mermaid
flowchart TD
    doctor["mise run doctor:platform<br/>preflight"] --> start["cluster:start · 6.2<br/>k3d + registry.localhost:5050"]
    start --> install["platform:install · 6.2<br/>pinned kagent chart"]
    install --> build["platform:dev · 6.2<br/>build & push images · 6.1"]
    build --> agent["BYO Agent + ModelConfig · 6.3"]
    build --> mcp["read-only MCP server · 6.4"]
    build --> gw["agentgateway + NetworkPolicy · 6.5"]
    agent --> pf["kubectl port-forward :3001"]
    mcp --> pf
    gw --> pf
```

**Diagram in words:** Run the platform doctor, start k3d and its local registry, install kagent, then let Skaffold build and push the images. That build creates the BYO Agent, read-only MCP server, and agentgateway/NetworkPolicy path. A temporary port-forward to agentgateway `:3001` is the host entry point.

The loop runs the other way too: `k3d cluster stop local` between sessions returns the memory without destroying anything, and `mise run cluster:start` resumes the same cluster ([6.2. Platform Install](./6.2.%20Platform%20Install.md#how-do-you-stop-the-cluster-between-sessions) owns both).

## What would plain Kubernetes use instead of kagent?

A plain Deployment is enough when no agent-specific reconciliation is useful.

| Choice                                                                                         | What it owns                                                                        | What you gain or lose                                                                                                                                |
| ---------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| kagent BYO `Agent`                                                                             | Agent description, image deployment, model reference, and controller reconciliation | The course can inspect an agent-native custom resource, but accepts an alpha API and its narrower deployment schema.                                 |
| [Kubernetes Deployment](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/) | Declarative Pod and ReplicaSet updates using stable built-in APIs                   | You gain the full Deployment schema and fewer controllers, but lose kagent's agent inventory, `ModelConfig` relationship, and agent-specific status. |

The application image and A2A contract do not care which controller owns the Pod. Use the built-in Deployment unless the agent-native resource and controller behavior earn their extra API surface.

## Which page owns which platform manifest?

Every platform concern has one owning manifest, so a broken rollout has one place to look.

This chapter covers:

- **[6.0. Platform](./6.0. Platform.md)** _(hands-on)_: Understand process-to-cluster ownership and prove base-versus-overlay render propagation.
- **[6.1. Containers](./6.1. Containers.md)** _(hands-on)_: Build the non-root agent image, then scan the exact artifact you built.
- **[6.2. Platform Install](./6.2. Platform Install.md)** _(hands-on)_: Create the tracked cluster, install kagent, and start the workloads with Skaffold.
- **[6.3. Platform Agents](./6.3. Platform Agents.md)** _(reference)_: Read the hardened BYO `Agent` and the `ModelConfig` that points it at the gateway.
- **[6.4. Platform Tools](./6.4. Platform Tools.md)** _(reference)_: Move the six read-only tools into their own in-cluster MCP deployment.
- **[6.5. Platform Gateway](./6.5. Platform Gateway.md)** _(reference)_: Keep agentgateway private behind network policy, and keep its secrets encrypted in git.
- **[6.6. Platform Delivery](./6.6. Platform Delivery.md)** _(hands-on)_: Back up the state, drill a restore, plan optional GKE, and tear down safely.
- **[6.7. Promotion and Rollback](./6.7. Promotion and Rollback.md)** _(hands-on)_: Review source evidence before promotion, then use an immutable image digest as the rollback surface.

Each page also owns the manifests below, so a symptom maps to one file:

| Sub-page                                                        | What it adds                                                  | Owning manifest(s)                                             |
| --------------------------------------------------------------- | ------------------------------------------------------------- | -------------------------------------------------------------- |
| [6.0. Platform](./6.0. Platform.md)                             | Agents as Kubernetes workloads; the shared base and overlays  | `infra/k8s/base/kustomization.yaml`                            |
| [6.1. Containers](./6.1. Containers.md)                         | The multi-stage, digest-pinned agent image                    | `agents/python/Dockerfile`                                     |
| [6.2. Platform Install](./6.2. Platform Install.md)             | Cluster/registry, kagent, and the Skaffold development loop   | `infra/k3d.yaml`, `infra/helmfile.yaml`, `infra/skaffold.yaml` |
| [6.3. Platform Agents](./6.3. Platform Agents.md)               | The hardened BYO `Agent` and gateway `ModelConfig`            | `infra/kagent/agent.yaml`, `modelconfig.yaml`                  |
| [6.4. Platform Tools](./6.4. Platform Tools.md)                 | The read-only MCP server and its governed `RemoteMCPServer`   | `infra/k8s/base/mcp.yaml`, `infra/kagent/toolserver.yaml`      |
| [6.5. Platform Gateway](./6.5. Platform Gateway.md)             | The private data plane, network policy, and workload identity | `infra/k8s/base/network-policies.yaml` + overlays              |
| [6.6. Platform Delivery](./6.6. Platform Delivery.md)           | State recovery, the OpenTofu GKE plan, and teardown           | `infra/scripts/`, `infra/gcp/`                                 |
| [6.7. Promotion and Rollback](./6.7. Promotion and Rollback.md) | Source evaluation before the image build/deploy handoff       | `scripts/promote.sh`                                           |

## What changes between the local and GKE overlays?

Three profiles reuse one base and make their model, identity, storage, and egress differences explicit.

**Kustomize** renders YAML from a shared `base/` plus per-environment `overlays/` patches; `kubectl kustomize <dir>` prints the result.

| Profile        | Model route                                            | Provider credential                                  | Runtime task                        |
| -------------- | ------------------------------------------------------ | ---------------------------------------------------- | ----------------------------------- |
| `local-gemini` | Gemini API through agentgateway                        | `gemini-provider` Secret mounted only by the gateway | `mise run platform:dev`             |
| `local`        | Host Ollama or deterministic fake through agentgateway | No upstream provider key                             | `mise run platform:dev:ollama`      |
| `gke`          | Vertex Gemini through agentgateway                     | Workload Identity                                    | Approved `mise run gke:deploy` only |

The main local profile extends `local`, replacing its model configuration, key mount, and model egress port. Both local profiles include Prometheus and Alertmanager; GKE leaves those services to an external operator. Local MLflow artifacts use a PVC; GKE uses GCS. The A2A and MCP contracts remain shared, but the rendered manifests are not byte-identical.

Both hosted profiles consume provider quota. Only `local` can use the deterministic fake; a successful fake-backed platform check does not prove either Gemini route. [6.5. Platform Gateway](./6.5.%20Platform%20Gateway.md) owns networking and [6.6. Platform Delivery](./6.6.%20Platform%20Delivery.md) owns the optional cloud boundary.

## What breaks first, and where do you look?

The same handful of failures recur across this chapter and the next. Start with configuration, then use logs and tests to distinguish it from an application failure.

Each row below is a symptom you can observe, the misconfiguration that usually causes it, and the page that owns the fix:

| Symptom                                               | Likely cause                                                                                                                              | Where to look                                                                                                     |
| ----------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| No traces appear in MLflow                            | An `http/protobuf` client points at `:4317` instead of `:4318`, or `OTEL_EXPORTER_OTLP_ENDPOINT` is unset entirely                        | [7.1. Tracing](../7. Observability/7.1. Tracing.md#how-do-you-point-a-host-agent-at-the-collector)                |
| Agent card fails to resolve though the pod is healthy | `AGENT_A2A_HOST` was left at `0.0.0.0` or the loopback default in-cluster, so the card advertises an uncallable URL                       | [6.3. Platform Agents](./6.3. Platform Agents.md#why-does-the-agent-advertise-a-different-a2a-host-than-it-binds) |
| Dashboards are flat / a port-forward returns nothing  | Host Compose and the in-cluster stack were started together and bound the same local ports                                                | [6.2. Platform Install](./6.2. Platform Install.md#how-do-you-start-the-local-kubernetes-workloads)               |
| Agent turns fail in k3d                               | Missing Gemini Secret or provider quota; on the optional Ollama profile, a loopback-only model listener                                   | [6.2. Platform Install](./6.2. Platform Install.md#how-do-you-start-the-local-kubernetes-workloads)               |
| Eval evidence vanished                                | `MLFLOW_TRACKING_URI` was unset, so `mise run eval:mlflow` wrote to the local `evals/mlflow.db` no one else sees                          | [7.0. Reproducibility](../7. Observability/7.0. Reproducibility.md#how-do-you-select-the-mlflow-destination)      |
| Pods stay `Pending`, or a container dies with `137`   | The machine is out of memory: host Compose and the in-cluster stack are running together, or the model plus k3s exceeds what the host has | [6.2. Platform Install](./6.2. Platform Install.md#what-do-you-do-when-the-machine-runs-out-of-memory)            |

## What proves this chapter worked?

One command renders and validates all three overlays offline: no live cluster, no GCP project, no model.

```bash
mise run check:infra
```

It runs `scripts/check-infra.sh`. That script builds each overlay with `kubectl kustomize`, then validates every object with `kubeconform` and `kube-linter` — a schema checker and a best-practice linter. It also diagnoses both Skaffold profiles, lints the helmfile, and runs `tofu validate` against the GKE plan.

The script also runs `tflint` on that plan, so it needs the `opentofu` and `tflint` binaries pinned in `mise.toml`. `mise run doctor:platform` checks for neither, so this gate can fail on a machine whose doctor is green.

For a faster spot check, render each overlay the way [6.0. Platform](./6.0. Platform.md) does and diff the output; its checkpoint owns those two commands. The local overlay also adds the Prometheus/Alertmanager stack the GKE overlay omits.

The chapter's required outcome is local. GCP stays at `tofu plan`: [6.6. Platform Delivery](./6.6. Platform Delivery.md) walks the plan and the teardown, and no cloud resource is created without a later, explicit approval.

**You are done when:**

- `mise run doctor:platform` exits 0.
- `mise run check:infra` exits 0, having rendered and validated `local`, `local-gemini`, and `gke`.
- You can name, for any of the eight sub-pages, the manifest it owns.
- You can say why no cluster exists yet, and which page creates one.
- You finished the required drill in [6.0. Platform](./6.0.%20Platform.md#your-turn-how-do-you-prove-a-manifest-change-reaches-the-render): your base edit showed up in both renders, your overlay edit in one, `mise run check:infra` refused the pinned model value, and `git restore infra/k8s` put the tree and the gate back.
- Without reopening Chapter 5, you can name the three protocols agentgateway fronts and say why their ClusterIP Services expose no public endpoint.

Continue to [6.0. Platform](./6.0.%20Platform.md) when `mise run check:infra` passes without a cluster, a GCP project, or a model.
