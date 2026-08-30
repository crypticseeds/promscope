// H3 workload: session churn. Every iteration acts as a brand-new client
// that initializes and asks one question - and deliberately NEVER sends
// DELETE, unlike the spec-compliant agent-turn.js. That is the point: in
// stateful mode each iteration strands a session in server memory until
// SessionTimeout (30m) reaps it; in stateless mode there is nothing to
// strand. Pair with scripts/memwatch.sh sampling the replicas' RSS.
import http from 'k6/http';
import { check } from 'k6';

const BASE = __ENV.BASE_URL || 'http://nginx:8090';

const HDRS = {
  'Content-Type': 'application/json',
  Accept: 'application/json, text/event-stream',
};

export const options = {
  scenarios: {
    churn: {
      executor: 'constant-vus',
      vus: Number(__ENV.VUS || 20),
      duration: __ENV.DURATION || '120s',
    },
  },
};

export default function () {
  const init = http.post(
    `${BASE}/mcp`,
    JSON.stringify({
      jsonrpc: '2.0',
      id: 1,
      method: 'initialize',
      params: {
        protocolVersion: '2025-06-18',
        capabilities: {},
        clientInfo: { name: 'k6-churn', version: '0' },
      },
    }),
    { headers: HDRS },
  );
  check(init, { 'initialize 200': (r) => r.status === 200 });

  const sid = init.headers['Mcp-Session-Id'] || init.headers['mcp-session-id'];
  const sess = sid ? { 'Mcp-Session-Id': sid } : {};
  http.post(
    `${BASE}/mcp`,
    JSON.stringify({ jsonrpc: '2.0', id: 2, method: 'ping' }),
    { headers: Object.assign({}, HDRS, sess) },
  );
  // No DELETE, no think time: maximize distinct sessions per minute.
}
