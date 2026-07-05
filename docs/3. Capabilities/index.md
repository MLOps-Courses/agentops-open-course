---
description: Give the agent real powers — tools, skills, MCP, memory, workflows, and A2A — with clean, packaged code.
---

# 3. Capabilities

Your agent can now hold a conversation ([Chapter 2](../2. Agents/)); this chapter gives it real powers. You package the code cleanly, then add one capability per page — tools to act, skills to know procedures, MCP to share tools across processes, memory to recall runbooks, workflows to run repeatable pipelines, and A2A to delegate to specialists.

Every capability lands in the Ops Copilot, so this chapter _is_ the reference agent's build-out. Each maps directly to a row of the Ops Copilot capability table: function tools (3.1), MCP (3.3), memory/RAG (3.4), workflows (3.5), and A2A/multi-agent (3.6). Both language tracks stay in lockstep — the code appears in Python/Go tabs, the concepts are shared.

This chapter covers:

- **[3.0. Packaging](./3.0. Packaging.md)**: Structure the agent as a Python package (uv) and a Go module.
- **[3.1. Tools](./3.1. Tools.md)**: Typed function tools over the incidents database, and OpenAPI toolsets.
- **[3.2. Skills](./3.2. Skills.md)**: Agent Skills for progressive-disclosure instructions.
- **[3.3. MCP](./3.3. MCP.md)**: Model Context Protocol servers and clients — one server, both tracks.
- **[3.4. Memory](./3.4. Memory.md)**: Long-term knowledge over runbooks with keyword retrieval, a stepping stone to RAG.
- **[3.5. Workflows](./3.5. Workflows.md)**: The triage → diagnose → recommend pipeline on the 2.0 graph runtime.
- **[3.6. A2A](./3.6. A2A.md)**: The Agent2Agent protocol, sub-agents, and delegation.
