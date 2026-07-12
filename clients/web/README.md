# Ops Copilot web client

A single-file A2A browser client for the course's Ops Copilot: one `index.html` with vanilla JavaScript, no build step, no framework, and no external requests (it works offline). It is teaching material for the _client_ side of the [A2A protocol](https://a2a-protocol.org/): agent-card discovery, `message/stream` (SSE) with a `message/send` fallback, task-state rendering, and the human-approval round-trip for guarded actions. MIT licensed (see [`../LICENSE`](../LICENSE)).

## What it does

1. Fetches and displays the agent card from `GET /.well-known/agent-card.json`.
1. Sends messages with `message/stream` when the card advertises streaming, and falls back to `message/send` (a single blocking JSON-RPC response) otherwise.
1. Renders incremental `status-update` and `artifact-update` events, with a distinct badge per task state (`submitted`, `working`, `input-required`, `completed`, `failed`, ...).
1. Surfaces a guarded action (`restart_service`, `resolve_incident`) as an explicit approval form: the task pauses in `input-required`, and the reply is a `FunctionResponse` data part carrying `{"confirmed": true, "payload": {"rationale": "..."}}` on the same task. The agent refuses approvals without a rationale, so the form requires one.

## How to run it

1. Start the A2A server: `cd agents/python && mise run a2a` (raw `:8080`).
1. Start the host gateway: `mise run gateway:host` from the repository root (A2A route on `:3001`).
1. Serve this directory: `python3 -m http.server 8001 --directory clients/web` from the repository root.
1. Open `http://localhost:8001`, keep the base URL `http://localhost:3001`, and press Connect.

Point the client at agentgateway `:3001` — the governed data plane — not the raw application port `:8080`.

## CORS: why the browser needs a gateway policy

The page runs on origin `http://localhost:8001` and calls `http://localhost:3001`, so the browser enforces CORS. Neither the raw A2A server (Starlette, no CORS middleware: `OPTIONS` returns `405`, no `Access-Control-Allow-Origin`) nor the a2a/rate-limit policies alone emit CORS headers. agentgateway `1.3.1` provides a route-level `cors` policy; add it to the `:3001` route in `infra/agentgateway/host/config.yaml` alongside the existing policies:

```yaml
cors:
  allowOrigins:
    - "http://localhost:8001"
  allowMethods:
    - GET
    - POST
    - OPTIONS
  allowHeaders:
    - content-type
```

With this policy the gateway answers the preflight itself (`200` with `access-control-allow-*` headers) and stamps `access-control-allow-origin` on card, RPC, and SSE responses. `allowOrigins` is an exact-match list: open the page on the exact origin you allow (`http://localhost:8001`, not `http://127.0.0.1:8001`).

## Limitations

1. Lab-only: no authentication, no TLS, loopback addresses — consistent with the course's no-public-endpoint stance.
1. One conversation per page load; it does not list or resume tasks after a reload (the server keeps them in `.state/runtime.db`).
1. Text parts only (the card advertises `text/plain`); file parts are not rendered.
1. Token-level streaming appears only when the server runs with `AGENT_A2A_STREAMING=true`; by default SSE carries whole events.
