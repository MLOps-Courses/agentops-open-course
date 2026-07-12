// A2A conversation path: JSON-RPC `message/send` through the agentgateway A2A
// listener. EVERY ITERATION RUNS A FULL AGENT TURN — model calls included.
//
// ============================== COST WARNING ==============================
// A local Qwen3-4B on Ollama takes seconds to minutes per turn and saturates
// your CPU/GPU; a hosted model bills real tokens for every iteration. This is
// NOT a path to load-test with many VUs: the model dominates latency by
// orders of magnitude, so high concurrency only proves that inference is the
// bottleneck — expensively. Keep the defaults (1 VU, 3 iterations) and treat
// this script as a latency *sample*, not a capacity probe.
// ==========================================================================
//
// Target (host quickstart default): http://localhost:3001/ — agentgateway
// proxies to the raw A2A server on :8080 and rate-limits at 60 requests/min.
// setup() fetches the agent card first so a stopped stack fails fast before
// any model tokens are spent.
//
// Safety: only point this script at your own local stack.
//
// Environment overrides:
//   A2A_URL     A2A JSON-RPC endpoint, default http://localhost:3001/
//   PROMPT      user message, default a cheap read-only triage question
//   VUS         concurrent conversations, default 1 — raise deliberately
//   ITERATIONS  total turns, default 3
//   TIMEOUT     per-request timeout, default 180s (cold model loads are slow)

import http from 'k6/http';
import { check, fail } from 'k6';

const A2A_URL = __ENV.A2A_URL || 'http://localhost:3001/';
const PROMPT = __ENV.PROMPT || 'List the open incidents.';
const TIMEOUT = __ENV.TIMEOUT || '180s';

export const options = {
  vus: Number(__ENV.VUS || 1),
  iterations: Number(__ENV.ITERATIONS || 3),
  thresholds: {
    // Budget aligned with the shipped AgentTurnLatencyP95High alert (15s):
    // a starting point — slower hardware will breach it, which is the lesson.
    http_req_failed: ['rate<0.01'],
    'http_req_duration{op:message_send}': ['p(95)<15000'],
  },
};

export function setup() {
  const cardUrl = A2A_URL.replace(/\/$/, '') + '/.well-known/agent-card.json';
  const res = http.get(cardUrl);
  if (res.status !== 200) {
    fail(`agent card fetch failed with HTTP ${res.status}: start the stack before spending model time`);
  }
}

export default function () {
  const payload = {
    jsonrpc: '2.0',
    id: crypto.randomUUID(),
    method: 'message/send',
    params: {
      message: {
        kind: 'message',
        role: 'user',
        messageId: crypto.randomUUID(),
        parts: [{ kind: 'text', text: PROMPT }],
      },
    },
  };
  const res = http.post(A2A_URL, JSON.stringify(payload), {
    headers: { 'Content-Type': 'application/json' },
    timeout: TIMEOUT,
    tags: { op: 'message_send' },
  });
  let message = null;
  try {
    message = JSON.parse(String(res.body || ''));
  } catch (error) {
    message = null;
  }
  check(res, {
    'message/send is 200': (r) => r.status === 200,
    'JSON-RPC result present': () => Boolean(message && message.result && !message.error),
    'result is a task or message': () =>
      Boolean(message && message.result && (message.result.kind === 'task' || message.result.kind === 'message')),
  });
}
