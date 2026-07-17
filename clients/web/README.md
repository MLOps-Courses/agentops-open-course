# AgentOps Agent web client

A single-file A2A browser client for the course's AgentOps Agent: one `index.html` with vanilla JavaScript, no build step, no framework, and no external requests (it works offline). It is teaching material for the _client_ side of the [A2A protocol](https://a2a-protocol.org/): agent-card discovery, `message/stream` (SSE) with a `message/send` fallback, task-state rendering, and the human-approval round-trip for guarded actions. MIT licensed (see [`../LICENSE`](../LICENSE)).

## What it does

1. Fetches and displays the agent card from `GET /.well-known/agent-card.json`.
1. Sends messages with `message/stream` when the card advertises streaming, and falls back to `message/send` (a single blocking JSON-RPC response) otherwise.
1. Parses SSE incrementally with CRLF, LF, or CR record separators; the locked A2A server emits CRLF by default.
1. Renders incremental `status-update` and `artifact-update` events, with a distinct badge per task state (`submitted`, `working`, `input-required`, `completed`, `failed`, ...).
1. Surfaces a guarded action (`restart_service`, `resolve_incident`) as an explicit approval form: the task pauses in `input-required`, keeps the preceding evidence/tool results visible, repeats the exact action arguments, and explains that execution re-reads current state while the write transaction validates the target. The reply is a `FunctionResponse` data part carrying `{"confirmed": true, "payload": {"rationale": "..."}}` on the same task. The agent refuses approvals without a rationale, so the form requires one.

## How to run it

1. Start the A2A server: `cd agents/python && mise run a2a` (raw `:8080`).
1. Start the digest-pinned host gateway wrapper: `mise run gateway:host` from the repository root (loopback A2A route on `:3001`).
1. Serve this directory: `mise run client:web` from the repository root.
1. Open `http://localhost:8001`, keep the base URL `http://localhost:3001`, and press Connect.

Point the client at agentgateway `:3001` — the governed data plane — not the raw application port `:8080`.

## CORS: why the browser needs a gateway policy

The page runs on origin `http://localhost:8001` and calls `http://localhost:3001`, so the browser enforces CORS. Neither the raw A2A server (Starlette, no CORS middleware: `OPTIONS` returns `405`, no `Access-Control-Allow-Origin`) nor the a2a/rate-limit policies alone emit CORS headers. The checked-in host and Kubernetes gateway profiles therefore include this route policy:

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

The gateway answers the preflight itself (`200` with `access-control-allow-*` headers) and stamps `access-control-allow-origin` on card, RPC, and SSE responses. `allowOrigins` is an exact-match list: open the page on the checked-in origin (`http://localhost:8001`, not `http://127.0.0.1:8001`). Do not replace it with a wildcard when adding credentials.

## Limitations

1. Lab-only: no authentication, no TLS, loopback addresses — consistent with the course's no-public-endpoint stance.
1. The default A2A runtime records a synthetic `A2A_USER_<context-id>` approver. This proves confirmation continuity, not authenticated human identity.
1. One conversation per page load; it does not list or resume tasks after a reload (the server keeps them in `.state/runtime.db`).
1. Text parts only (the card advertises `text/plain`); file parts are not rendered.
1. Token-level streaming appears only when the server runs with `AGENT_A2A_STREAMING=true`; by default SSE carries whole events.
