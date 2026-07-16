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

import http from 'k6/http';
import { check } from 'k6';

const MCP_HEALTH_URL = __ENV.MCP_HEALTH_URL || 'http://localhost:8000/healthz';
const A2A_HEALTH_URL = __ENV.A2A_HEALTH_URL || 'http://localhost:8080/healthz';
const GATEWAY_HEALTH_URL = __ENV.GATEWAY_HEALTH_URL || 'http://localhost:3001/healthz';

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
    // Latency budgets — starting points for localhost, tune to your hardware.
    http_req_failed: ['rate<0.01'],
    'http_req_duration{op:raw_health}': ['p(95)<50'],
    'http_req_duration{op:gateway_health}': ['p(95)<100'],
  },
};

export function rawHealth() {
  for (const url of [MCP_HEALTH_URL, A2A_HEALTH_URL]) {
    const res = http.get(url, { tags: { op: 'raw_health' } });
    check(res, { 'raw healthz is 200': (r) => r.status === 200 });
  }
}

export function gatewayHop() {
  const res = http.get(GATEWAY_HEALTH_URL, { tags: { op: 'gateway_health' } });
  check(res, { 'gateway healthz is 200': (r) => r.status === 200 });
}
