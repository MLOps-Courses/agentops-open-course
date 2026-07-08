# Agents

The AgentOps Open Course reference agent — the **Ops Copilot**, an on-call assistant that triages and resolves incidents over a 100% local, bundled dataset. It is implemented as a self-contained Python project plus a shared dataset.

- [`python/`](./python) — Google ADK 2.0, Python (uv, ruff, ty, pytest).
- [`data/`](./data) — the shared dataset: `incidents.db` (SQLite), `runbooks/`, `skills/`, logs.

The agent reads the shared `data/`, so it runs fully offline. It starts on **native Gemini** (API key or ADC) in Chapters 2–4, then reaches any provider — including local Ollama — through **agentgateway** in Chapter 5, with no SDK changes.

## Capabilities (and where they live)

| Capability                        | Python (`python/src/agent/`)    | Chapter |
| --------------------------------- | ------------------------------- | ------- |
| Agent + persona + model           | `agent.py`, `config.py`         | 2       |
| Data-access layer                 | `data.py`                       | 3.1     |
| Function tools                    | `tools.py`                      | 3.1     |
| Skills (progressive disclosure)   | `skills.py`                     | 3.2     |
| MCP server + client               | `mcp_server.py`/`mcp_client.py` | 3.3     |
| Memory / RAG (runbooks)           | `memory.py`                     | 3.4     |
| Workflow (triage→diagnose→…)      | `workflow.py`                   | 3.5     |
| Multi-agent delegation / A2A      | `delegation.py`                 | 3.6     |
| Guardrails + HITL actions         | `guardrails.py`/`actions.py`    | 4.5     |
| PII redaction (Presidio)          | `pii.py`                        | 4.5     |
| Evaluations (`adk eval` + MLflow) | `../evals/`                     | 4.4     |
| Telemetry (OpenTelemetry)         | `telemetry.py`                  | 7.1     |

## Run it

```bash
cd python && mise run install && cp .env.example .env   # add your key
mise run web            # ADK dev UI     ·  mise run run          (terminal)
mise run test           # unit tests     ·  mise run eval         (adk eval)
mise run eval:mlflow    # MLflow judges  ·  mise run redteam      (garak, via uvx)
```

Licensed under the [MIT License](./LICENSE).
