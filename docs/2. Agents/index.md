---
description: Build a small Google ADK agent, then use the completed reference to understand models, instructions, sessions, and development checks.
---

# 2. Agents

!!! abstract "In one glance"

    - **You will:** Build a small agent and find the reference page for each part of its behavior.
    - **You need:** The learner runtime installed; model credentials only for optional interactive work.
    - **Time:** about 8 minutes, orientation.

**Part I — Agent development.** Work through [2.6. Workshop](../2.%20Agents/2.6.%20Workshop.md) and consult this chapter when the exercise introduces its subject. Python fundamentals are assumed; no Kubernetes knowledge is needed here.

## What will you understand in this chapter?

Start with your small `learner_agent`, then compare it with the completed AgentOps Agent as you add capabilities.

The reference's `composition.py` assembles `root_agent` from a model, instructions, and tools. Its enclosing `App` registers `AgentOpsPolicyPlugin` once to govern every model and tool call, including sub-agents and workflow stages.

??? note "Deeper: where does this agent go after Chapter 2?"

    The completed reference already contains the course's capabilities. Chapter 3 explains them, Chapter 4 validates them, Chapters 5 and 6 deploy the application, and Chapter 7 observes it.

    [Chapter 3](../3. Capabilities/) deepens its tools, knowledge, workflows, and delegation; [Chapter 8.7](../8.%20Community/8.7.%20Capstone.md) asks you to adapt these boundaries to your own domain.

Begin with First Agent and the Workshop. Consult the other pages when an exercise needs their concepts; the reference development loop requires the larger contributor installation.

- **[2.0. Concepts](./2.0. Concepts.md)** _(concept)_: The ADK 2.x building blocks — Agent, Runner, Session, Events, Tools, and the graph Workflow.
- **[2.1. First Agent](./2.1. First Agent.md)** _(hands-on)_: Create a learner-owned agent, check it offline, and optionally inspect a model conversation.
- **[2.2. Models](./2.2. Models.md)** _(reference)_: The optional Ollama contract and the default native Gemini branch.
- **[2.3. Instructions](./2.3. Instructions.md)** _(hands-on)_: The system instruction, its enforcement map, and a deterministic red/green trajectory contract.
- **[2.4. Sessions](./2.4. Sessions.md)** _(reference)_: Persistent ADK sessions, **A2A** tasks (units of work exchanged between agents across process boundaries), lifecycle ownership, and resettable runtime state.
- **[2.5. Dev Loop](./2.5. Dev Loop.md)** _(hands-on)_: Offline gates, interactive modes, model-backed evaluations, and failure diagnosis.

You can complete the exercises offline. When model access is available, use a small conversation to compare intended behavior with the events you observe.

## Which page owns which part of the agent?

The `Agent(...)` call in `composition.py` names each part of the reference agent. Each part is taught by exactly one sub-page, so when a behavior surprises you, there is one page and one module to open.

Concretely, each field of `root_agent` traces to one owner:

| Sub-page                                    | What it teaches                                | Owning module / symbol                                        |
| ------------------------------------------- | ---------------------------------------------- | ------------------------------------------------------------- |
| [2.0. Concepts](./2.0. Concepts.md)         | The ADK runtime loop and its object vocabulary | `google.adk` (framework)                                      |
| [2.1. First Agent](./2.1. First Agent.md)   | Composing and running `root_agent`             | `composition.py` (composition root)                           |
| [2.2. Models](./2.2. Models.md)             | Provider selection behind `model=`             | `model.py` `build_model`, `config.py` `ModelProvider`         |
| [2.3. Instructions](./2.3. Instructions.md) | The persona and rules behind `instruction=`    | `composition.py` `INSTRUCTION` / `_instruction`               |
| [2.4. Sessions](./2.4. Sessions.md)         | Persistent sessions and A2A task state         | `server.py` `DatabaseSessionService`, `config.py` `state_dir` |
| [2.5. Dev Loop](./2.5. Dev Loop.md)         | The offline gates and interactive run modes    | `mise.toml` tasks                                             |

Tools and policy hooks are named here, not taught here. Owned by [Chapter 3](../3. Capabilities/) and [4.5. Guardrails](../4.%20Quality/4.5.%20Guardrails.md).

??? note "Deeper: the same map as a diagram, and who owns tools and policy"

    This diagram maps the anatomy to its owners:

    ```mermaid
    flowchart TD
        concepts["Runtime concepts · 2.0<br/>Agent · Runner · Session · Events"]
        subgraph agent["root_agent — assembled in composition.py · 2.1"]
            model["model = build_model() · 2.2"]
            instr["instruction = _instruction() · 2.3"]
            tools["tools = [reads, actions, memory, skills]<br/>policy plugin on App · Ch. 3 / 4.5"]
        end
        runtime["Persistent runtime · 2.4<br/>DatabaseSessionService · A2A tasks · server.py"]
        loop["Dev loop · 2.5<br/>mise run test · run · web · a2a"]
        concepts --> agent
        agent --> runtime
        loop -. iterates .-> agent
    ```

    **Diagram in words:** Runtime concepts lead to one agent assembled from a model, instruction, tools, and an app-level policy plugin; persistent runtime and the dev loop surround it.

    The `tools=` list and app plugin belong to later chapters: 2.1 shows the wiring, [Chapter 3](../3. Capabilities/) owns each tool, and [4.5. Guardrails](../4.%20Quality/4.5.%20Guardrails.md) owns policy. This page only names the seams.

- **[2.6. Workshop](./2.6. Workshop.md)** _(hands-on)_: Build eight cumulative Python checkpoints with offline checks and worked solutions.

## What proves this chapter worked?

Check your first agent from the repository root without starting a model:

```bash
mise run lab -- check 1
```

Create the step through [2.1. First Agent](./2.1.%20First%20Agent.md) first. The check proves construction, not model behavior. Continue building your own tools through [2.6. Workshop](./2.6.%20Workshop.md).

??? note "Deeper: how do you validate the completed reference?"

    After the contributor installation, the full reference suite verifies deterministic behavior and enforces 95% combined line-and-branch coverage:

    ```bash
    cd agents/python
    mise run test
    ```

    For a narrower check after a model/config edit, use `uv run pytest tests/test_model.py tests/test_config.py`. Do not rerun the full suite for every page you read.

    [2.3. Instructions](./2.3.%20Instructions.md#your-turn-which-eval-case-catches-a-rule-you-delete) provides a reference-editing drill. [2.5. Dev Loop](./2.5.%20Dev%20Loop.md) owns the full development workflow and its separate model-backed evidence.

**You are done when:**

- Your learner-owned step 1 passes its offline check.
- You can distinguish your cumulative exercise from the completed reference application.
- You know where to find the model, instruction, session, and development-loop explanations.
- You can explain what an offline construction check cannot establish about a model's answer.

Continue to [2.1. First Agent](./2.1.%20First%20Agent.md) to create your learner-owned file, or consult [2.0. Concepts](./2.0.%20Concepts.md) first if the ADK vocabulary is unfamiliar.
