# AgentOps Reference Agent — Go

The reference agent for the [AgentOps Open Course](https://agentops-open-course.fmind.dev/), built with **Google ADK 2.0** (Go, `google.golang.org/adk/v2`). Its Python counterpart lives in [`../python`](../python).

## Quickstart

```bash
mise run install            # go mod tidy
cp .env.example .env        # then add your GOOGLE_API_KEY
mise run run -- web webui   # ADK web UI (browser)
mise run run -- console     # terminal REPL
```

Run modes (`full` launcher): `console` (default), `web webui` (browser UI), `web api` (REST), `web a2a` (A2A). `web` alone errors — it needs a sub-server keyword.

## Layout

- `cmd/agent/main.go` — entry point; runs the agent via the ADK `full` launcher.
- `internal/agent/` — the agent definition (`New`).
- `internal/config/` — typed configuration (model, data dir).
- `internal/tools/` — function tools (Ch. 3.1).
- `internal/memory/` — long-term memory / RAG (Ch. 3.4).
- `tests/` — integration tests. `evals/` — evaluation datasets (Ch. 4.4).

Tasks: `mise run format`, `mise run check`, `mise run test`, `mise run build`.
