// Baseline latency floor: the raw health endpoints, plus one low-rate hop
// through agentgateway so the pure proxy overhead becomes measurable.
//
// Targets (host quickstart defaults):
//   - MCP server   GET http://localhost:8000/healthz  (mise run mcp:http)
//   - A2A server   GET http://localhost:8080/healthz  (mise run a2a)
//   - agentgateway GET http://localhost:3001/healthz  (proxied to the A2A server)
//
// Safety: only point these scripts at your own local stack. The :3001 hop
// shares the gateway A2A token bucket (60 requests/min), so the gateway
// scenario stays far below that limit by design.
//
// Environment overrides:
//   RATE      raw iterations per second (each iteration sends 2 GETs), default 10
//   DURATION  scenario duration, default 30s
//   MCP_HEALTH_URL / A2A_HEALTH_URL / GATEWAY_HEALTH_URL
//   MCP_HOST_HEADER  authority to present to the MCP server, unset by default.
//     The MCP server refuses any Host not on MCP_ALLOWED_HOSTS with 421, so a
//     port-forward dialled as localhost:8000 never reaches a handler. Set this
//     to an allowlisted authority (agentops-mcp:8000 in the cluster) and the
//     URL can stay on loopback; see load/README.md.

import http from 'k6/http';
import { check } from 'k6';

const MCP_HEALTH_URL = __ENV.MCP_HEALTH_URL || 'http://localhost:8000/healthz';
const A2A_HEALTH_URL = __ENV.A2A_HEALTH_URL || 'http://localhost:8080/healthz';
const GATEWAY_HEALTH_URL = __ENV.GATEWAY_HEALTH_URL || 'http://localhost:3001/healthz';
const MCP_HOST_HEADER = __ENV.MCP_HOST_HEADER;

export const options = {
  scenarios: {
    raw_health: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE || 10), // iterations/s; each iteration sends 2 GETs
      timeUnit: '1s',
      duration: __ENV.DURATION || '30s',
      preAllocatedVUs: 10,
      maxVUs: 50,
      exec: 'rawHealth',
    },
    gateway_hop: {
      // 30 requests/min keeps a 30-token margin under the gateway A2A rate limit.
      executor: 'constant-arrival-rate',
      rate: 30,
      timeUnit: '1m',
      duration: __ENV.DURATION || '30s',
      preAllocatedVUs: 2,
      maxVUs: 4,
      exec: 'gatewayHop',
    },
  },
  thresholds: {
    // Latency budgets, measured rather than guessed: the laptop run recorded
    // in 7.2. Monitoring read raw p(95)=7.13ms against gateway p(95)=10.4ms,
    // which is where that page's three milliseconds of proxy overhead come
    // from. 50ms is about seven times that raw baseline and 100ms about ten
    // times the gateway one, so each budget catches a collapse rather than
    // that 3ms — the overhead is read off the summary, not enforced here. They
    // stay two separate numbers so a gateway regression is measured against
    // the gateway instead of hiding under a floor shared with the raw
    // endpoints. Tune both to your hardware.
    http_req_failed: ['rate<0.01'],
    'http_req_duration{op:raw_health}': ['p(95)<50'],
    'http_req_duration{op:gateway_health}': ['p(95)<100'],
  },
};

export function rawHealth() {
  // Only the MCP server enforces a Host allowlist, so only its request carries
  // the override; an empty object leaves the request exactly as it was before.
  const targets = [
    [MCP_HEALTH_URL, MCP_HOST_HEADER ? { Host: MCP_HOST_HEADER } : {}],
    [A2A_HEALTH_URL, {}],
  ];
  for (const [url, headers] of targets) {
    const res = http.get(url, { headers, tags: { op: 'raw_health' } });
    check(res, { 'raw healthz is 200': (r) => r.status === 200 });
  }
}

export function gatewayHop() {
  const res = http.get(GATEWAY_HEALTH_URL, { tags: { op: 'gateway_health' } });
  check(res, { 'gateway healthz is 200': (r) => r.status === 200 });
}
