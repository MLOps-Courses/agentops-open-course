# Load tests

Grafana k6 scenarios that stress the **platform** paths of the AgentOps Agent stack and sample the **model** path. The walkthrough lives in the course page [7.2. Monitoring](../docs/7.%20Observability/7.2.%20Monitoring.md).

Run them through the repository tasks, from the repository root. Each scenario reads its knobs from the environment, so overrides go in front of the task:

```bash
mise run load:health
DURATION=15s RATE=12 mise run load:mcp
ITERATIONS=1 mise run load:a2a
```

k6 is open source under AGPL-3.0, consistent with the rest of the stack, and is deliberately absent from `[tools]` in `mise.toml`: each task fetches a pinned ephemeral binary instead of installing one permanently. That is all a task is — `load:health` runs `mise x k6@<pinned version> -- k6 run load/health.js`, and the version lives in the `load:*` tasks in `mise.toml`. Use the raw form only to pass a k6 flag the task does not forward.

The pinned container image is the alternative when you would rather not fetch a binary (host networking so `localhost` targets resolve); keep its tag equal to the version those tasks pin:

```bash
docker run --rm --network host -v "$PWD/load:/scripts:ro" grafana/k6:2.1.0 run /scripts/health.js
```

## Scenarios

1. `health.js` — raw `/healthz` on MCP `:8000` and A2A `:8080`, plus a low-rate hop through agentgateway `:3001`. Establishes the latency floor and the pure gateway overhead.
1. `mcp-read.js` — MCP streamable HTTP `tools/call` (`list_incidents`) through the gateway `:3000`. Measures gateway + MCP server + SQLite without any model call.
1. `a2a-send.js` — one bounded A2A `message/send` conversation through the gateway `:3001`. Every iteration requires a completed, non-empty result with no structured ADK error; an HTTP 200 carrying a failed task does not pass. The defaults are 1 VU and 3 model-backed turns.
1. `fake_model.py` — a deterministic OpenAI-compatible upstream packaged as an isolated PEP 723 script. Run the same A2A scenario against it to isolate agent/gateway overhead from inference latency.

Each script encodes its latency budget and successful checks as k6 `thresholds`, so a breached budget or failed check fails the run. MCP JSON-RPC errors and tool `isError` results fail even when HTTP returns 200. All budgets are localhost starting points — tune them to your hardware instead of deleting them.

For A2A, a successful result is either a non-empty `Message`, or a `Task` whose state is `completed` and whose status message or artifact contains text. The scenario rejects missing JSON-RPC results, failed/incomplete tasks, empty output, and `metadata.adk_error_code`.

## Prerequisites

Follow [5.1. Gateway Setup](../docs/5.%20Gateway/5.1.%20Gateway%20Setup.md) for MCP, gateway, and A2A startup, including the A2A process's gateway environment. The default gateway uses hosted Gemini; health and MCP scenarios make no model calls, while A2A consumes model quota. Run `mise run smoke:host` before adding load. On Kubernetes, port-forward agentgateway and the raw services first and override the `*_URL` environment variables.

For the fake-model comparison, stop the A2A process and current gateway, and free `:11434` if your Ollama process is using it. In separate terminals, run root `mise run model:fake` and `mise run gateway:host:ollama`, then restart A2A from `agents/python/`:

```bash
AGENT_MODEL_PROVIDER=openai-compatible \
AGENT_MODEL=qwen3:4b-instruct \
OPENAI_BASE_URL=http://127.0.0.1:4000/v1 \
OPENAI_API_KEY=local-gateway \
AGENT_MCP_URL=http://127.0.0.1:3000/mcp \
AGENT_A2A_STREAMING=false \
mise run a2a
```

The explicit Ollama profile routes to the fake on `:11434`; the default Gemini profile would still call the hosted provider. The fake deliberately refuses streaming and returns fixed text without tools, so this measures a simpler platform path, not model quality or an identical real-model trajectory. Three turns are a smoke sample, not a stable p95 estimate. Stop the fake, A2A, and gateway when finished, then restore your chosen provider configuration.

## Safety

1. Only target your own local stack. Never point these scripts at shared, third-party, or production endpoints — that is a denial-of-service attempt, not a lab.
1. The shipped gateway rate limits (120 MCP and 60 A2A requests/min) are part of the platform: the defaults stay under them, and any HTTP 429 means you measured your own rate limiter.
1. The A2A scenario spends real model time (and real tokens on hosted providers). Raise `VUS`/`ITERATIONS` deliberately, never by default.
