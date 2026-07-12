// MCP read path: JSON-RPC `tools/call` through the agentgateway MCP listener.
//
// Target (host quickstart default): http://localhost:3000/mcp — agentgateway
// proxies to the raw FastMCP server on :8000/mcp. The script speaks MCP
// streamable HTTP: each VU performs the `initialize` handshake once, echoes
// the negotiated `MCP-Protocol-Version` and any `Mcp-Session-Id` the gateway
// issues, then loops `tools/call` on a read-only tool. Responses may arrive as
// plain JSON or as an SSE `data:` frame; both are parsed.
//
// The shipped gateway policy rate-limits this listener to 120 requests/min.
// The default rate (60 tool calls/min plus 2 handshake requests per VU) stays
// under that budget on purpose: pushing harder measures your own rate limiter
// (HTTP 429), not the platform. Raise `maxTokens` in
// infra/agentgateway/host/config.yaml to turn this into a real capacity probe,
// or point MCP_URL at the raw server (http://localhost:8000/mcp) to isolate
// FastMCP + SQLite from the gateway.
//
// Safety: only point this script at your own local stack.
//
// Environment overrides:
//   MCP_URL   MCP endpoint, default http://localhost:3000/mcp
//   TOOL      read-only tool name, default list_incidents
//   RATE      tool calls per minute, default 60
//   DURATION  scenario duration, default 60s

import http from 'k6/http';
import { check, fail } from 'k6';
import { Counter } from 'k6/metrics';

const MCP_URL = __ENV.MCP_URL || 'http://localhost:3000/mcp';
const TOOL = __ENV.TOOL || 'list_incidents';

const rateLimited = new Counter('mcp_rate_limited');

export const options = {
  scenarios: {
    mcp_read: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE || 60),
      timeUnit: '1m',
      duration: __ENV.DURATION || '60s',
      preAllocatedVUs: 5,
      maxVUs: 10,
    },
  },
  thresholds: {
    // Latency budget — a starting point for localhost, tune to your hardware.
    http_req_failed: ['rate<0.01'],
    'http_req_duration{op:tools_call}': ['p(95)<250'],
    mcp_rate_limited: ['count==0'], // any 429 means the gateway budget, not the platform, was measured
  },
};

// Per-VU session state: k6 gives every VU its own copy of module scope.
let session = null;

function post(payload, extraHeaders, op) {
  const headers = Object.assign(
    {
      'Content-Type': 'application/json',
      // MCP streamable HTTP requires accepting both JSON and SSE responses.
      Accept: 'application/json, text/event-stream',
    },
    extraHeaders,
  );
  return http.post(MCP_URL, JSON.stringify(payload), { headers, tags: { op } });
}

// A streamable HTTP response is either a JSON body or an SSE stream whose
// final `data:` line carries the JSON-RPC response message.
function parseMessage(res) {
  const contentType = res.headers['Content-Type'] || '';
  const body = String(res.body || '');
  if (contentType.includes('text/event-stream')) {
    const dataLines = body.split('\n').filter((line) => line.startsWith('data:'));
    if (dataLines.length === 0) {
      return null;
    }
    return JSON.parse(dataLines[dataLines.length - 1].slice('data:'.length));
  }
  try {
    return JSON.parse(body);
  } catch (error) {
    return null;
  }
}

function sessionHeaders() {
  const headers = { 'MCP-Protocol-Version': session.protocolVersion };
  if (session.id) {
    headers['Mcp-Session-Id'] = session.id; // stateless upstreams issue none
  }
  return headers;
}

function handshake() {
  const init = post(
    {
      jsonrpc: '2.0',
      id: `init-${__VU}`,
      method: 'initialize',
      params: {
        protocolVersion: '2025-06-18',
        capabilities: {},
        clientInfo: { name: 'agentops-k6-mcp-read', version: '1.0.0' },
      },
    },
    {},
    'initialize',
  );
  if (init.status === 429) {
    rateLimited.add(1);
  }
  if (init.status !== 200) {
    fail(`initialize failed with HTTP ${init.status}: is the gateway + MCP stack running?`);
  }
  const message = parseMessage(init);
  if (!message || message.error || !message.result) {
    fail(`initialize returned an error: ${init.body}`);
  }
  session = {
    id: init.headers['Mcp-Session-Id'] || null,
    protocolVersion: message.result.protocolVersion,
  };
  const initialized = post({ jsonrpc: '2.0', method: 'notifications/initialized' }, sessionHeaders(), 'initialized');
  check(initialized, { 'initialized notification accepted': (r) => r.status === 202 });
}

export default function () {
  if (!session) {
    handshake();
  }
  const res = post(
    {
      jsonrpc: '2.0',
      id: `${__VU}-${__ITER}`,
      method: 'tools/call',
      params: { name: TOOL, arguments: {} },
    },
    sessionHeaders(),
    'tools_call',
  );
  if (res.status === 429) {
    rateLimited.add(1);
  }
  if (res.status === 404) {
    session = null; // spec: 404 means the session expired — re-initialize next iteration
  }
  const message = parseMessage(res);
  check(res, {
    'tools/call is 200': (r) => r.status === 200,
    'tools/call has a JSON-RPC result': () => Boolean(message && message.result && !message.error),
    'tool did not report an error': () => Boolean(message && message.result && message.result.isError !== true),
  });
}
