# AgentOps Reference Agent (Python)

The **Ops Copilot** is the executable reference for the [AgentOps Open Course](https://agentops-open-course.fmind.dev/). It uses Google ADK for the agent runtime, Pydantic for trusted boundaries, MCP and A2A for interoperability, SQLite for deterministic local state, and OpenTelemetry for runtime signals.

## Quickstart

Install and verify without a model:

```bash
mise run install
mise run check
mise run test
```

The test suite is deterministic, network-independent after installation, and enforces at least 95% branch coverage.

For native Gemini, create the **repository-root** environment file, not one inside this directory:

```bash
cp ../../.env.example ../../.env
# Set GOOGLE_API_KEY in ../../.env
mise run web
```

For the local Qwen3 path, run Ollama and agentgateway as described in Chapter 5, then set:

```bash
AGENT_GATEWAY_ENABLED=true
AGENT_MODEL=qwen3:4b
OPENAI_BASE_URL=http://localhost:4000/v1
OPENAI_API_KEY=agentgateway
AGENT_MCP_URL=http://localhost:3000/mcp
```

## Runtime contracts

- `root_agent` is discovered by the ADK CLI from `src/agent`.
- `python -m agent.server` serves the A2A application on `0.0.0.0:8080` by default.
- Sessions and A2A tasks use persistent SQLite services under `.state/`.
- `AGENT_DATA_DIR` points to immutable seed data; `AGENT_STATE_DIR` holds its writable runtime copy.
- `AGENT_GATEWAY_ENABLED=false` selects native Gemini; `true` requires an OpenAI-compatible gateway URL and marker/token.
- `AGENT_MCP_URL` selects streamable HTTP MCP; without it, the client starts the local stdio server.
- HTTP MCP keeps DNS-rebinding protection enabled; `MCP_ALLOWED_HOSTS` can narrow its explicit authority allowlist.
- message content capture in telemetry is disabled by default.

## Layout

```text
src/agent/
  agent.py        Root agent, tools, callbacks, and instruction
  config.py       Typed environment settings
  model.py        Native Gemini or OpenAI-compatible gateway model
  models.py       Trusted domain and tool boundary types
  data.py         Seed-to-runtime state and data access
  tools.py        Read-only incident, service, and log tools
  skills.py       Allowlisted skill discovery and loading
  mcp_server.py   stdio or streamable HTTP MCP server
  mcp_client.py   ADK MCP toolset selection
  memory.py       Runbook retrieval
  workflow.py     Explicit triage workflow
  delegation.py   Specialist delegation
  guardrails.py   Input and action policy
  actions.py      Approved writes and append-only audit
  pii.py          Presidio request/response/tool-output callbacks
  telemetry.py    Privacy-preserving OpenTelemetry setup
  server.py       Persistent A2A application factory and process
evals/            ADK trajectories and MLflow evaluation
tests/            Offline unit and local integration tests
```

## Tasks

| Task                   | Network/model use                  | Purpose                                                                   |
| ---------------------- | ---------------------------------- | ------------------------------------------------------------------------- |
| `mise run format`      | None                               | Format imports and Python.                                                |
| `mise run check`       | Vulnerability database may refresh | Check metadata, lock, format, lint, types, and dependencies.              |
| `mise run test`        | None                               | Run branch-covered offline tests.                                         |
| `mise run redteam`     | None                               | Run deterministic adversarial regression cases; not a live-model scanner. |
| `mise run run`         | Model                              | Run the ADK terminal UI.                                                  |
| `mise run web`         | Model                              | Run the ADK developer UI.                                                 |
| `mise run mcp`         | None                               | Serve MCP over stdio.                                                     |
| `mise run mcp:http`    | None                               | Serve MCP over HTTP at `127.0.0.1:8000`.                                  |
| `mise run a2a`         | Depends on requests                | Serve persistent A2A on port `8080`.                                      |
| `mise run eval`        | Model                              | Gate recorded ADK tool trajectories.                                      |
| `mise run eval:mlflow` | Model; judge optional              | Log cases, prompt lineage, scorers, and optional judge results to MLflow. |
| `mise run data:reset`  | None                               | Delete disposable `.state/` and restore on next use.                      |

## Reset and cleanup

```bash
mise run data:reset
mise run clean
```

`data:reset` never deletes or edits `../data/incidents.db`. Do not place durable or sensitive information in the course runtime database.

## License

The code is [MIT licensed](../LICENSE). The bundled course content and external model/provider services have separate licenses and terms.
