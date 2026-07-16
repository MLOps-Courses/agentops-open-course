# Load tests

Grafana k6 scenarios that stress the **platform** paths of the Ops Copilot stack and sample the **model** path. The walkthrough lives in the course page [7.2. Monitoring](../docs/7.%20Observability/7.2.%20Monitoring.md).

k6 is open source under AGPL-3.0, consistent with the rest of the stack. Run a pinned binary without installing anything permanent:

```bash
mise x k6@2.1.0 -- k6 run load/health.js
```

or use the pinned container image (host networking so `localhost` targets resolve):

```bash
docker run --rm --network host -v "$PWD/load:/scripts:ro" grafana/k6:2.1.0 run /scripts/health.js
```

## Scenarios

1. `health.js` — raw `/healthz` on MCP `:8000` and A2A `:8080`, plus a low-rate hop through agentgateway `:3001`. Establishes the latency floor and the pure gateway overhead.
1. `mcp-read.js` — MCP streamable HTTP `tools/call` (`list_incidents`) through the gateway `:3000`. Measures gateway + FastMCP + SQLite without any model call.
1. `a2a-send.js` — one bounded A2A `message/send` conversation through the gateway `:3001`. Every iteration runs a full agent turn with model calls: 1 VU, 3 iterations by default.
1. `fake_model.py` — a deterministic OpenAI-compatible upstream packaged as an isolated PEP 723 script. Run the same A2A scenario against it to isolate agent/gateway overhead from inference latency.

Each script encodes its latency budget as k6 `thresholds`, so a breached budget fails the run. All budgets are localhost starting points — tune them to your hardware instead of deleting them.

## Prerequisites

The host quickstart must be running: `mise run mcp:http` and `mise run a2a` from `agents/python/`, the loopback wrapper `mise run gateway:host` from the repository root, and Ollama serving `qwen3:4b` for the A2A scenario. Run `mise run smoke:host` before adding load. On Kubernetes, port-forward agentgateway and the raw services first and override the `*_URL` environment variables.

For the fake-model comparison, stop Ollama so port `11434` is free, run `mise run model:fake`, and restart the A2A process with `AGENT_MODEL_PROVIDER=openai-compatible` and `OPENAI_BASE_URL=http://127.0.0.1:4000/v1`. The existing host and k3d gateway profiles already route model calls to that host port, so the A2A script and every other layer stay identical. The fake deliberately refuses streaming; keep `AGENT_A2A_STREAMING=false` so the experiment changes only inference.

## Safety

1. Only target your own local stack. Never point these scripts at shared, third-party, or production endpoints — that is a denial-of-service attempt, not a lab.
1. The shipped gateway rate limits (120 MCP and 60 A2A requests/min) are part of the platform: the defaults stay under them, and any HTTP 429 means you measured your own rate limiter.
1. The A2A scenario spends real model time (and real tokens on hosted providers). Raise `VUS`/`ITERATIONS` deliberately, never by default.
