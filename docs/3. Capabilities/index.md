---
description: Give the agent real powers — tools, skills, MCP, memory, workflows, and A2A — with clean, packaged code.
---

# 3. Capabilities

Your agent can now hold a conversation ([Chapter 2](../2. Agents/)); this chapter gives it real powers. You package the code cleanly, then add one capability per page — tools to act, skills to know procedures, MCP to share tools across processes, memory to recall runbooks, workflows to run repeatable pipelines, and A2A to delegate to specialists.

Each capability lands as its own module in the reference agent's codebase, so this chapter _is_ the Ops Copilot's build-out — mapping to rows of its capability table: function tools (3.1), skills (3.2), MCP (3.3), memory/RAG (3.4), workflows (3.5), and A2A/multi-agent (3.6). Some wire directly into the root agent (tools, memory, guardrails); others (skills, MCP, workflow, delegation) are standalone constructs you run and test on their own.

This chapter covers:

- **[3.0. Packaging](./3.0. Packaging.md)**: Structure the agent as a Python package (uv).
- **[3.1. Tools](./3.1. Tools.md)**: Typed function tools over the incidents database, and OpenAPI toolsets.
- **[3.2. Skills](./3.2. Skills.md)**: Agent Skills for progressive-disclosure instructions.
- **[3.3. MCP](./3.3. MCP.md)**: Model Context Protocol servers and clients.
- **[3.4. Memory](./3.4. Memory.md)**: Long-term knowledge over runbooks with keyword retrieval, a stepping stone to RAG.
- **[3.5. Workflows](./3.5. Workflows.md)**: The triage → diagnose → recommend pipeline on the 2.0 graph runtime.
- **[3.6. A2A](./3.6. A2A.md)**: The Agent2Agent protocol, sub-agents, and delegation.
