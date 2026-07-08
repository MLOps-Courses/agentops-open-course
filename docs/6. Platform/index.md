---
description: Run agents as Kubernetes workloads with kagent — a CNCF project — on a local k3d cluster.
---

# 6. Platform

Run agents as Kubernetes workloads with [kagent](https://kagent.dev/) — a CNCF project — on a local k3d cluster. This chapter takes the Ops Copilot from a process on your laptop to a real cluster deployment: you build its image, install the kagent control plane, declare the agent, its model, and its tools as custom resources, front it with the agentgateway data plane, and wire the whole thing into a repeatable delivery loop.

This chapter covers:

- **[6.0. Platform](./6.0. Platform.md)**: Agents as Kubernetes workloads, and a kagent overview.
- **[6.1. Containers](./6.1. Containers.md)**: Build and publish agent images with a multi-stage Dockerfile.
- **[6.2. Platform Install](./6.2. Platform Install.md)**: Install kagent on k3d with the `kagent` CLI or Helm.
- **[6.3. Platform Agents](./6.3. Platform Agents.md)**: The `Agent` (`type: BYO`) and `ModelConfig` CRDs; deploy the ADK agent.
- **[6.4. Platform Tools](./6.4. Platform Tools.md)**: The `ToolServer` and `RemoteMCPServer` CRDs, plus `MCPServer` via bundled kmcp.
- **[6.5. Platform Gateway](./6.5. Platform Gateway.md)**: Integrate agentgateway as the platform data plane.
- **[6.6. Platform Delivery](./6.6. Platform Delivery.md)**: CI/CD, GitOps, and helmfile.
