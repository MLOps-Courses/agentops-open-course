---
description: Run the same private AgentOps data plane on local k3d and an optional, explicitly planned GKE lab.
---

# 6. Platform

## Where will you run the agent?

Move the validated host data plane to local k3d with [kagent](https://kagent.dev/), then inspect an optional GKE plan without applying it. The same locked agent/MLflow images, Kustomize base, gateway ports, MCP path, A2A service, state PVCs, and OTel pipeline run in both environments; only overlays, registry, model backend, and workload identity change.

This chapter covers:

- **[6.0. Platform](./6.0. Platform.md)**: Agents as Kubernetes workloads, and a kagent overview.
- **[6.1. Containers](./6.1. Containers.md)**: Build and publish agent images with a multi-stage Dockerfile.
- **[6.2. Platform Install](./6.2. Platform Install.md)**: Create the tracked k3d/registry and install the pinned slim kagent chart.
- **[6.3. Platform Agents](./6.3. Platform Agents.md)**: Declare the hardened single-replica BYO Agent, gateway ModelConfig, and state PVC.
- **[6.4. Platform Tools](./6.4. Platform Tools.md)**: Deploy the read-only MCP server and register only its governed endpoint.
- **[6.5. Platform Gateway](./6.5. Platform Gateway.md)**: Run the private data plane with network policy and GKE workload identity.
- **[6.6. Platform Delivery](./6.6. Platform Delivery.md)**: Use Skaffold locally, review the OpenTofu GKE plan, and tear down safely.

The chapter's required outcome is local. GCP remains at `tofu plan`; no cloud resource is created without a later explicit approval.
