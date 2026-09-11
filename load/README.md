# Load tests

Grafana k6 scenarios that stress the **platform** paths of the AgentOps Agent stack and sample the **model** path. The walkthrough lives in the course page [7.2. Monitoring](../content/7.%20Observability/7.2.%20Monitoring.md).

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
1. `mcp-read.js` — MCP streamable HTTP `tools/call` (`list_incidents`) through the gateway `:3000`. Measures gateway + the Go MCP server + SQLite without any model call.
1. `a2a-send.js` — one bounded A2A `message/send` conversation through the gateway `:3001`. Every iteration requires a completed, non-empty result with no structured ADK error; an HTTP 200 carrying a failed task does not pass. The defaults are 1 VU and 3 model-backed turns.
1. `model:fake` — a deterministic OpenAI-compatible Go server. Run the same A2A scenario against it to isolate agent/gateway overhead from inference latency.

Each script encodes its latency budget as k6 `thresholds`, so a breached budget fails the run. All budgets are localhost starting points — tune them to your hardware instead of deleting them.

For A2A, a successful result is either a non-empty `Message`, or a `Task` whose state is `completed` and whose status message or artifact contains text. The scenario rejects missing JSON-RPC results, failed/incomplete tasks, empty output, and `metadata.adk_error_code`.

## Prerequisites

The host quickstart must be running: `mise run mcp:http` and `mise run a2a` from `agents/go/`, the loopback wrapper `mise run gateway:host` from the repository root, and Ollama serving `qwen3:4b-instruct` for the A2A scenario. Run `mise run smoke:host` before adding load. On Kubernetes, port-forward agentgateway and the raw services first and override the `*_URL` environment variables.

The raw MCP server is the one target a port-forward does not reach on its own URL. `MCP_ALLOWED_HOSTS` in `infra/k8s/base/mcp.yaml` deliberately names only the gateway and Service authorities, and the environment override replaces the binary's own defaults rather than extending them, so the loopback entries that exist for the host quickstart are not there. `kubectl port-forward` also bypasses the NetworkPolicy, which leaves that Host guard as the only layer in front of `:8000`. A client dialling `http://localhost:8000` presents `localhost:8000`, and the server answers 421 before any handler runs — the guard working, not a misconfiguration.

The fix is to present an authority that is on the list. Either alias it to loopback (`127.0.0.1 agentops-mcp` in `/etc/hosts`) and point the tool at `http://agentops-mcp:8000`, which the `agentops-mcp:*` entry matches, or leave the URL on loopback and set `MCP_HOST_HEADER`, which `health.js` and `mcp-read.js` send as the request Host and nothing else:

```bash
kubectl -n agentops port-forward svc/agentops-mcp 8000:8000
MCP_HOST_HEADER=agentops-mcp:8000 mise run load:health
MCP_URL=http://localhost:8000/mcp MCP_HOST_HEADER=agentops-mcp:8000 mise run load:mcp
```

Both scripts leave the header off entirely when the variable is unset, so the host quickstart — where the binary's default allowlist already covers loopback — is unaffected.

For the fake-model comparison, stop Ollama so port `11434` is free, run `mise run model:fake`, and restart A2A with `AGENT_MODEL_PROVIDER=openai-compatible` and `OPENAI_BASE_URL=http://127.0.0.1:11434/v1`. The existing host and k3d gateway profiles already route model calls to that host port, so every other layer stays identical. The fake deliberately refuses streaming; keep `AGENT_A2A_STREAMING=false` so the experiment changes only inference.

## Scaling out

`infra/k8s/overlays/scale` runs the MCP read plane at two replicas behind a `HorizontalPodAutoscaler`; [6.9. Scale Out](../content/6.%20Platform/6.9.%20Scale%20Out.md) is the walkthrough. Point `mcp-read.js` at the scaled path the same way you point it anywhere else — through a port-forward of the gateway, or straight at the raw server to isolate the Go MCP process from the proxy:

```bash
MCP_URL=http://localhost:8000/mcp MCP_HOST_HEADER=agentops-mcp:8000 DURATION=30s RATE=6000 mise run load:mcp
```

`MCP_HOST_HEADER` is the raw-server requirement from Prerequisites: the port-forwarded server answers 421 to `localhost:8000` and never runs a handler, so a run without it measures the Host guard rather than the read path.

Two measured baselines from a developer laptop, so a breach on your hardware means something. They are the runs transcribed in 6.9, driven against two raw MCP processes on the host rather than against the scaled overlay on a cluster: one process served 100 `tools/call` per second at `p(95)=4.86ms`, and both processes driven concurrently at that rate — 200 per second in total — answered at `p(95)=6.15ms` and `p(95)=5.93ms`, with no failed request in any of them. Read the second pair against the first rather than on its own: the second process did not make anything faster. All three sit far inside the 250 ms budget in `mcp-read.js`, which is the useful finding rather than a footnote — the shipped gateway limit of 120 requests per minute binds long before the server does, so replication here buys availability, not throughput.

The A2A scenario is not a capacity probe and does not become one when the agent has more replicas: the agent stays single-replica for the reasons that page names, and its latency belongs to the model either way.

## Safety

1. Only target your own local stack. Never point these scripts at shared, third-party, or production endpoints — that is a denial-of-service attempt, not a lab.
1. The shipped gateway rate limits (120 MCP and 60 A2A requests/min) are part of the platform: the defaults stay under them, and any HTTP 429 means you measured your own rate limiter.
1. The A2A scenario spends real model time (and real tokens on hosted providers). Raise `VUS`/`ITERATIONS` deliberately, never by default.
