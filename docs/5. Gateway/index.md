---
description: Route and govern the agent's MCP, A2A, and model traffic through a local-first agentgateway data plane.
---

# 5. Gateway

## What will the gateway add?

Put **[agentgateway](https://agentgateway.dev)** between the agent and three boundaries: MCP read tools, A2A clients, and an OpenAI-compatible model endpoint. The host profile stays account-free with Ollama/Qwen3; Chapter 6 moves the same listener contract to k3d and optional GKE overlays.

!!! info "AAIF project"

    agentgateway was created by Solo.io and donated to the Linux Foundation; it is now an
    **[Agentic AI Foundation (AAIF)](https://aaif.io/projects/agentgateway/)** project. This
    chapter uses it as the connectivity and traffic-policy layer while keeping application approval and transactions in ADK/Python.

This chapter covers:

- **[5.0. Gateway](./5.0. Gateway.md)**: The connectivity and security problem agents face, and an agentgateway overview.
- **[5.1. Gateway Setup](./5.1. Gateway Setup.md)**: Run the digest-pinned gateway image through its loopback-only host wrapper.
- **[5.2. MCP Gateway](./5.2. MCP Gateway.md)**: Front exactly six reads with fail-closed authorization.
- **[5.3. A2A Gateway](./5.3. A2A Gateway.md)**: Route agent-to-agent traffic with the `a2a` route policy.
- **[5.4. Model Gateway](./5.4. Model Gateway.md)**: Stabilize the agent on one endpoint while choosing local Qwen3 or GKE Vertex Gemini upstream.
- **[5.5. Gateway Security](./5.5. Gateway Security.md)**: Active allowlists, limits, prompt guards, identity, and residual risk.
- **[5.6. Gateway Observability](./5.6. Gateway Observability.md)**: JSON logs, internal metrics, OTLP traces, and content-capture privacy.

The chapter checkpoint tests fail-closed MCP, A2A discovery, local model translation, prompt rejection, and telemetry through gateway ports only.
