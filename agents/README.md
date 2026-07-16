# Ops Copilot

The course reference system combines a self-contained Google ADK application with an immutable local dataset:

- [`python/`](./python) contains the typed agent, MCP and A2A servers, evaluations, and tests.
- [`data/`](./data) contains the SQLite seed, service logs, runbooks, and least-privilege Agent Skills.

The deterministic engineering path runs offline after dependencies are installed. The first interactive run uses the Apache-2.0 open-weight Qwen3 model directly through local Ollama. Chapter 5 changes the same OpenAI-compatible endpoint to agentgateway; native Gemini remains optional.

## Architecture

```mermaid
flowchart LR
    Client[A2A client] --> Server[A2A server]
    Server --> Agent[ADK root agent]
    Agent --> Read[Read tools]
    Agent --> Skills[Skill tools]
    Agent --> HITL[Approved write tools]
    Read --> State[(Runtime SQLite copy)]
    HITL --> State
    Agent --> MCP[MCP client/server]
    Agent --> Model[Local Ollama or agentgateway]
    Server --> OTel[OpenTelemetry]
```

## Capability map

| Capability                                       | Source                               | Course               |
| ------------------------------------------------ | ------------------------------------ | -------------------- |
| Agent, instructions, callbacks                   | `python/src/agent/agent.py`          | Chapter 2            |
| Typed configuration and model selection          | `config.py`, `model.py`, `models.py` | Chapters 2 and 5     |
| Immutable seed and runtime state                 | `data.py`, `data/`                   | Chapter 3            |
| Incident, service, and log tools                 | `tools.py`                           | Chapter 3.1          |
| Least-privilege Agent Skills                     | `skills.py`                          | Chapter 3.2          |
| MCP server and client                            | `mcp_server.py`, `mcp_client.py`     | Chapter 3.3          |
| Runbook retrieval                                | `memory.py`                          | Chapter 3.4          |
| Deterministic workflow                           | `workflow.py`                        | Chapter 3.5          |
| Delegation and A2A                               | `delegation.py`, `server.py`         | Chapters 3.6 and 6   |
| Approval, actions, append-only audit             | `guardrails.py`, `actions.py`        | Chapters 4.5 and 7.6 |
| Request, response, and tool-output PII callbacks | `pii.py`                             | Chapters 4.5 and 4.6 |
| ADK and MLflow evaluations                       | `python/evals/`                      | Chapters 4.4 and 7   |
| OTLP telemetry                                   | `telemetry.py`                       | Chapter 7.1          |

## Offline checkpoint

From the repository root:

```bash
mise install
mise run install
mise run check
mise run test
```

Tests enforce at least 95% branch coverage and do not call a model or cloud service.

## Run the account-free model path

Install Ollama, pull the model, and validate the staged prerequisite:

```bash
ollama pull qwen3:4b
mise run doctor:model
```

Then run from the Python agent directory:

```bash
cd agents/python
mise run run
```

The typed defaults are `AGENT_MODEL_PROVIDER=openai-compatible`, `AGENT_MODEL=qwen3:4b`, `OPENAI_BASE_URL=http://127.0.0.1:11434/v1`, and the non-secret `local-ollama` client marker. No provider account or `.env` file is required. [Chapter 5](../docs/5.%20Gateway/) changes the base URL to agentgateway so model policy and telemetry move outside the application.

For the optional native Gemini branch, set `AGENT_MODEL_PROVIDER=gemini`, an explicit Gemini model, and either a Gemini API key or Application Default Credentials in the repository-root `.env`.

## Licenses

Agent code is [MIT](./LICENSE). Model weights, SDKs, and services retain their own licenses and terms; see the course's provider chapter before redistributing an image or model.
