---
description: Run and understand the completed Google ADK 2.0 reference agent end to end on local Qwen3.
---

# 2. Agents

## What will you understand in this chapter?

This is where you run and understand the **Ops Copilot**, the completed reference agent carried through the whole course. You start from the runtime concepts, inspect the real composition root, then make model selection, instructions, persistent sessions, and the development loop explicit.

Work through the sections in order — the concepts come first, then the build:

- **[2.0. Concepts](./2.0. Concepts.md)**: The ADK 2.0 building blocks — Agent, Runner, Session, Events, Tools, and the graph Workflow.
- **[2.1. First Agent](./2.1. First Agent.md)**: Inspect and run the Ops Copilot end to end on local Qwen3.
- **[2.2. Models](./2.2. Models.md)**: Understand the default Ollama contract and the optional native Gemini branch.
- **[2.3. Instructions](./2.3. Instructions.md)**: The system instruction — persona, operating rules, grounding, and structured output.
- **[2.4. Sessions](./2.4. Sessions.md)**: Persistent ADK sessions, A2A tasks, lifecycle ownership, and resettable runtime state.
- **[2.5. Dev Loop](./2.5. Dev Loop.md)**: Offline gates, interactive modes, model-backed evaluations, and failure diagnosis.

By the end you can explain a typed agent with an explicit provider path, policy callbacks, a persistent A2A runtime, and a model-free verification gate. [Chapter 3](../3. Capabilities/) deepens its tools, knowledge, workflows, and delegation; [Chapter 8.7](../8.%20Community/8.7.%20Capstone.md) asks you to adapt these boundaries to your own domain.
