---
description: Run and understand the completed Google ADK 2.x reference agent end to end on local Qwen3.
---

# 2. Agents

!!! abstract "In one glance"

    - **You will:** See how the whole chapter fits together, then prove the agent assembles correctly without starting a model.
    - **You need:** Chapter 1 finished, with `mise run doctor` and `mise run doctor:model` passing.
    - **Time:** about 8 minutes, orientation.

## What will you understand in this chapter?

This chapter builds one object, `root_agent`. Every later chapter adds to that same object rather than replacing it.

That object is the **AgentOps Agent**, the single reference agent carried through the entire course. It is assembled once in `composition.py` as a plain ADK `Agent` value: a model, an instruction string, a flat tool list, and a set of **policy callbacks** — code the runtime runs around every model call and tool call.

??? note "Deeper: where does this agent go after Chapter 2?"

    Every later chapter instruments _this same object_: Chapter 3 hangs capabilities off it, Chapter 4 wraps it in quality gates, Chapters 5 and 6 put it behind a gateway and onto Kubernetes, and Chapter 7 observes it in production.

    [Chapter 3](../3. Capabilities/) deepens its tools, knowledge, workflows, and delegation; [Chapter 8.7](../8.%20Community/8.7.%20Capstone.md) asks you to adapt these boundaries to your own domain.

Read the sections by their kind, not just their order. **2.0 is conceptual**: the mental model you need before code makes sense. **2.1 and 2.5 are hands-on**: you run commands and see output. **2.2, 2.3, and 2.4 are reference**: the model, instruction, and runtime pieces you consult as you build.

- **[2.0. Concepts](./2.0. Concepts.md)** _(concept)_: The ADK 2.x building blocks — Agent, Runner, Session, Events, Tools, and the graph Workflow.
- **[2.1. First Agent](./2.1. First Agent.md)** _(hands-on)_: Inspect and run the AgentOps Agent end to end on local Qwen3.
- **[2.2. Models](./2.2. Models.md)** _(reference)_: The default Ollama contract and the optional native Gemini branch.
- **[2.3. Instructions](./2.3. Instructions.md)** _(hands-on)_: The system instruction, its enforcement map, and a deterministic red/green trajectory contract.
- **[2.4. Sessions](./2.4. Sessions.md)** _(reference)_: Persistent ADK sessions, **A2A** tasks (units of work exchanged between agents across process boundaries), lifecycle ownership, and resettable runtime state.
- **[2.5. Dev Loop](./2.5. Dev Loop.md)** _(hands-on)_: Offline gates, interactive modes, model-backed evaluations, and failure diagnosis.

By the end you will have run the agent on a model on your own laptop. You will know which file picks the model, which string sets its behavior, where a conversation is stored, and which command proves it all works without a model.

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

Tools and callbacks are named here, not taught here. Owned by [Chapter 3](../3. Capabilities/) and [4.5. Guardrails](../4.%20Quality/4.5.%20Guardrails.md).

??? note "Deeper: the same map as a diagram, and who owns tools and callbacks"

    This diagram maps the anatomy to its owners:

    ```mermaid
    flowchart TD
        concepts["Runtime concepts · 2.0<br/>Agent · Runner · Session · Events"]
        subgraph agent["root_agent — assembled in composition.py · 2.1"]
            model["model = build_model() · 2.2"]
            instr["instruction = _instruction() · 2.3"]
            tools["tools = [reads, actions, memory, skills]<br/>+ policy callbacks · Ch. 3 / 4.5"]
        end
        runtime["Persistent runtime · 2.4<br/>DatabaseSessionService · A2A tasks · server.py"]
        loop["Dev loop · 2.5<br/>mise run test · run · web · a2a"]
        concepts --> agent
        agent --> runtime
        loop -. iterates .-> agent
    ```

    The `tools=` list and the `before_*`/`after_*` callbacks belong to later chapters: 2.1 shows you the wiring, but [Chapter 3](../3. Capabilities/) owns each tool and [4.5. Guardrails](../4.%20Quality/4.5.%20Guardrails.md) owns the callback policy. This page only names the seams.

## What proves this chapter worked?

Two things prove the chapter. The first is one command, and it never starts a model:

```bash
cd agents/python
mise run test
```

That is the offline test suite. The second is the required drill in [2.3. Instructions](./2.3.%20Instructions.md#your-turn-which-eval-case-catches-a-rule-you-delete), which does use your local model: you delete one instruction rule and find out what notices. Do the command first — a red suite makes the drill unreadable. It constructs the agent, resolves its configuration, and exercises model and session wiring without a running model or network.

The whole run can take several minutes depending on the machine and cache state. It ends with a coverage total checked against the enforced 95% threshold, then a pytest `passed` line. Nothing in it needs a model or a network, so a red line is a real failure rather than a missing piece of setup.

A green run proves the agent is assembled correctly, not that it reasons well. Model-backed evaluation is a separate evidence lane, owned by [2.5. Dev Loop](./2.5. Dev Loop.md).

??? note "Deeper: can you test the model and config wiring on its own?"

    That is the umbrella gate (`uv run pytest` over the full suite). To verify just this chapter's seams in isolation, run the model and config tests directly:

    ```bash
    uv run pytest tests/test_model.py tests/test_config.py
    ```

    That focused subset exits cleanly and gives fast feedback. The repository-wide 95% branch-coverage gate belongs to `mise run test`, which adds the coverage flags around the complete suite.

    Those cover provider resolution and the fail-fast cross-field checks in `config.py` — a bad `AGENT_MODEL_PROVIDER` combination fails at construction with a message that names the fix, not deep inside a turn. Model-backed behavior stays a separate evidence path ([2.5. Dev Loop](./2.5. Dev Loop.md)'s `mise run eval`), because a green offline suite proves the agent is assembled correctly, not that it reasons well.

**You are done when:**

- `mise run test` finishes in `agents/python` with no failures and a coverage total above the enforced 95% threshold.
- You finished the required drill in [2.3. Instructions](./2.3.%20Instructions.md#your-turn-which-eval-case-catches-a-rule-you-delete): the focused offline contract went red without the runbook rule, green after the scoped restore, and the live-model comparison remained optional evidence.
- You can name the one object every later chapter adds to, and the file that assembles it.
- You can say which sub-page owns the model, which owns the instruction, which owns the session store, and which owns the dev loop.
- You know which two pages ask you to run something and which three you will come back to as reference.
- Without reopening Chapter 1: you can name the command that proves the environment offline and the directory you run it from, and say why a passing `mise run test` reads no `.env`.

Continue to [2.0. Concepts](./2.0.%20Concepts.md) when `mise run test` passes on your machine, because every page after this one assumes the agent already builds.
