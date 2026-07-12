---
description: Build and run your first Google ADK 2.0 agent, end to end, on a native Gemini model.
---

# 2. Agents

## What will you build in this chapter?

This is where you build the **Ops Copilot**, the reference agent carried through the whole course. You start from the runtime concepts, compose the real root agent, then make model selection, instructions, persistent sessions, and the development loop explicit.

Read [2.0. Concepts](./2.0. Concepts.md) first, then [2.1. First Agent](./2.1. First Agent.md), then work through the following sections:

- **[2.0. Concepts](./2.0. Concepts.md)**: The ADK 2.0 building blocks — Agent, Runner, Session, Events, Tools, and the graph Workflow.
- **[2.1. First Agent](./2.1. First Agent.md)**: Build and run the Ops Copilot end to end.
- **[2.2. Models](./2.2. Models.md)**: Wire a native Gemini model, choose API-key or Vertex AI auth, and defer other providers to the gateway.
- **[2.3. Instructions](./2.3. Instructions.md)**: The system instruction — persona, operating rules, grounding, and structured output.
- **[2.4. Sessions](./2.4. Sessions.md)**: Persistent ADK sessions, A2A tasks, lifecycle ownership, and resettable runtime state.
- **[2.5. Dev Loop](./2.5. Dev Loop.md)**: Offline gates, interactive modes, model-backed evaluations, and failure diagnosis.

By the end you have a typed agent with an explicit native or gateway model path, policy callbacks, a persistent A2A runtime, and a model-free verification gate. [Chapter 3](../3. Capabilities/) deepens its tools, knowledge, workflows, and delegation.
