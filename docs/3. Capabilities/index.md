---
description: Give the agent real powers — tools, skills, MCP, memory, workflows, and A2A — with clean, packaged code.
---

# 3. Capabilities

## Which capabilities will you add?

Your agent can now hold a conversation ([Chapter 2](../2. Agents/)); this chapter gives it bounded capabilities. You package the code, add typed read tools and progressively disclosed procedures, route those reads over MCP when configured, retrieve reviewed runbooks, encode fixed control flow as a graph, and expose the agent over A2A.

Each capability has one owner in the reference package. Local development uses direct read tools; a configured `AGENT_MCP_URL` replaces those reads with the governed MCP toolset. Guarded write tools and skill loading stay in-process so approval and least-privilege context are preserved.

This chapter covers:

- **[3.0. Packaging](./3.0. Packaging.md)**: Structure the agent as a Python package (uv).
- **[3.1. Tools](./3.1. Tools.md)**: Typed read capabilities over validated, resettable incident state.
- **[3.2. Skills](./3.2. Skills.md)**: Agent Skills for progressive-disclosure instructions.
- **[3.3. MCP](./3.3. MCP.md)**: Model Context Protocol servers and clients.
- **[3.4. Memory](./3.4. Memory.md)**: Conversation, state, and deterministic runbook retrieval without conflating them.
- **[3.5. Workflows](./3.5. Workflows.md)**: The `triage -> diagnose -> recommend` graph with per-node least privilege.
- **[3.6. A2A](./3.6. A2A.md)**: In-process delegation versus the persistent networked agent contract.

The chapter checkpoint is the offline test suite for tools, skills, MCP, retrieval, workflows, delegation, and A2A server construction. Model-backed behavior remains a separate evaluation gate.
