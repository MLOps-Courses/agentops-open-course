# AgentOps Reference Agent — Python

The reference agent for the [AgentOps Open Course](https://agentops-open-course.fmind.dev/), built with **Google ADK 2.0** (Python). Its Go counterpart lives in [`../go`](../go).

## Quickstart

```bash
mise run install            # uv sync
cp .env.example .env        # then add your GOOGLE_API_KEY
mise run web                # ADK developer web UI  (adk web src)
mise run run                # run in the terminal   (adk run src/agent)
```

## Layout

- `src/agent/agent.py` — the agent (`root_agent`), discovered by the `adk` CLI.
- `src/agent/config.py` — typed settings (model, data dir).
- `src/agent/tools.py` — function tools (Ch. 3.1).
- `src/agent/memory.py` — long-term memory / RAG (Ch. 3.4).
- `tests/` — unit and integration tests. `evals/` — evaluation datasets (`adk eval`, Ch. 4.4).

Tasks: `mise run format`, `mise run check`, `mise run test`.
