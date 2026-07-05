---
description: Secure and connect the agent with agentgateway — the flagship AAIF project — running locally first.
---

# 5. Gateway

Secure and connect the agent with **[agentgateway](https://agentgateway.dev)** — the flagship AAIF project — running locally first, then promoted to Kubernetes in Chapter 6. The gateway sits between the agent and everything it talks to (MCP tools, other agents, and models) and centralises the production concerns — auth, rate limits, safety, and observability — in one config, for both the Python and Go tracks.

!!! info "AAIF project"

    agentgateway was created by Solo.io and donated to the Linux Foundation; it is now an
    **[Agentic AI Foundation (AAIF)](https://aaif.io/projects/agentgateway/)** project. This
    chapter is the course's dedicated promotion of it as the connectivity and security layer.

This chapter covers:

- **[5.0. Gateway](./5.0. Gateway.md)**: The connectivity and security problem agents face, and an agentgateway overview.
- **[5.1. Gateway Setup](./5.1. Gateway Setup.md)**: Install and run agentgateway locally with a single binary and a config file.
- **[5.2. MCP Gateway](./5.2. MCP Gateway.md)**: Front the Ops Copilot MCP server and multiplex servers into one endpoint.
- **[5.3. A2A Gateway](./5.3. A2A Gateway.md)**: Route agent-to-agent traffic with the `a2a` route policy.
- **[5.4. Model Gateway](./5.4. Model Gateway.md)**: Point the model endpoint at the gateway's `ai` backend to reach any provider, including local Ollama.
- **[5.5. Gateway Security](./5.5. Gateway Security.md)**: Authentication, authorization, rate limits, and prompt guards.
- **[5.6. Gateway Observability](./5.6. Gateway Observability.md)**: Gateway metrics, tracing, and the admin UI.
