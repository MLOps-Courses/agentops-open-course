# Agents

The AgentOps Open Course reference agent — the **Ops Copilot**, an on-call assistant that triages and resolves incidents over a 100% local, bundled dataset. It is implemented as two parallel **tracks** (one per language), each a self-contained project, plus a shared dataset.

- [`python/`](./python) — Google ADK 2.0, Python (uv, ruff, ty, pytest).
- [`go/`](./go) — Google ADK 2.0, Go (`google.golang.org/adk/v2`, golangci-lint, gotestsum).
- [`data/`](./data) — the shared dataset: `incidents.db` (SQLite), `runbooks/`, `skills/`, logs.

Both tracks stay in lockstep and read the same `data/`, so the agent behaves identically. They start on **native Gemini** (API key or ADC) in Chapters 2–4, then reach any provider — including local Ollama — through **agentgateway** in Chapter 5, with no SDK changes.

## Capabilities (and where they live)

| Capability                      | Python (`python/src/agent/`)    | Go (`go/internal/`)      | Chapter |
| ------------------------------- | ------------------------------- | ------------------------ | ------- |
| Agent + persona + model         | `agent.py`, `config.py`         | `agent/`, `config/`      | 2       |
| Data-access layer               | `data.py`                       | `data/`                  | 3.1     |
| Function tools                  | `tools.py`                      | `tools/`                 | 3.1     |
| Skills (progressive disclosure) | `skills.py`                     | `skills/`                | 3.2     |
| MCP server + client             | `mcp_server.py`/`mcp_client.py` | `mcpclient/`             | 3.3     |
| Memory / RAG (runbooks)         | `memory.py`                     | `memory/`                | 3.4     |
| Workflow (triage→diagnose→…)    | `workflow.py`                   | `workflow/`              | 3.5     |
| Multi-agent delegation / A2A    | `delegation.py`                 | `delegation/`            | 3.6     |
| Guardrails + HITL actions       | `guardrails.py`/`actions.py`    | `guardrails/`/`actions/` | 4.5     |
| Evaluations                     | `../evals/`                     | —                        | 4.4     |
| Telemetry (OpenTelemetry)       | `telemetry.py`                  | launcher (env)           | 7.1     |

## Run it

```bash
# Python
cd python && mise run install && cp .env.example .env   # add your key
mise run web            # ADK dev UI     ·  mise run run   (terminal)
mise run test           # unit tests     ·  mise run eval  (adk eval)

# Go
cd go && mise run install && cp .env.example .env        # add your key
mise run run -- web     # ADK web UI     ·  mise run run -- console
mise run test           # unit tests
```

Licensed under the [MIT License](./LICENSE).
