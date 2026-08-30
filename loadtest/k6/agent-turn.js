// promscope load test - the "agent turn" workload.
//
// One iteration = one agent interaction: initialize, then discover -> query
// -> range-query with think time. The script is mode-agnostic on purpose:
// if the server issues an Mcp-Session-Id (stateful mode), every subsequent
// request carries it - exactly what a spec-compliant client does. Behind
// round-robin with 2 replicas, that session id is unknown to the *other*
// replica ~50% of the time, and those requests fail with 404 "session not
// found". In stateless mode no session id exists and nothing can miss.
//
// Run it against the compose stack without installing k6:
//
//   docker run --rm -i --network promscope_default \
//     -e VUS=10 -e DURATION=60s grafana/k6 run - < loadtest/k6/agent-turn.js
//
// H1 comparison: run once with the stack stateless (default), once after
//   PROMSCOPE_STATELESS=false docker compose -f deploy/compose.yaml up -d
// and compare `session_not_found` / `agent_turn_failures`.
import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate } from 'k6/metrics';

const BASE = __ENV.BASE_URL || 'http://nginx:8090';

// Post-initialize requests rejected with 404 session-not-found: the H1 metric.
const sessionNotFound = new Rate('session_not_found');
// Any post-initialize request that did not produce a usable result.
const turnFailures = new Rate('agent_turn_failures');

const HDRS = {
  'Content-Type': 'application/json',
  Accept: 'application/json, text/event-stream',
};

export const options = {
  scenarios: {
    agents: {
      executor: 'constant-vus',
      vus: Number(__ENV.VUS || 10),
      duration: __ENV.DURATION || '60s',
    },
  },
  // Closed-loop by design (each VU waits for responses + think time). Known
  // limitation: under-reports latency spikes (coordinated omission). We
  // compare configurations relative to each other on one machine.
};

function post(payload, extraHeaders) {
  return http.post(`${BASE}/mcp`, JSON.stringify(payload), {
    headers: Object.assign({}, HDRS, extraHeaders),
  });
}

// Responses are SSE-framed ("data: {...}"); fall back to plain JSON.
function parseBody(res) {
  const body = String(res.body || '');
  const m = body.match(/^data: (.*)$/m);
  try {
    return JSON.parse(m ? m[1] : body);
  } catch (e) {
    return null;
  }
}

function sessionHeaderOf(res) {
  const sid = res.headers['Mcp-Session-Id'] || res.headers['mcp-session-id'];
  return sid ? { 'Mcp-Session-Id': sid } : {};
}

export default function () {
  // 1. Handshake. Always succeeds: whichever replica gets it owns the session.
  const init = post({
    jsonrpc: '2.0',
    id: 1,
    method: 'initialize',
    params: {
      protocolVersion: '2025-06-18',
      capabilities: {},
      clientInfo: { name: 'k6-agent', version: '0' },
    },
  });
  check(init, { 'initialize 200': (r) => r.status === 200 });

  // Spec-compliant clients echo the NEGOTIATED protocol version on every
  // subsequent request, and the session id when one was issued.
  const initBody = parseBody(init);
  const sess = sessionHeaderOf(init);
  if (initBody && initBody.result && initBody.result.protocolVersion) {
    sess['MCP-Protocol-Version'] = initBody.result.protocolVersion;
  }

  // Spec-compliant clients send this after initialize. In stateful mode it
  // round-robins like everything else - the handshake itself can strand.
  const done = post({ jsonrpc: '2.0', method: 'notifications/initialized' }, sess);
  sessionNotFound.add(done.status === 404);

  // 2. The agent turn: discover, ask, ask over time.
  const calls = [
    { name: 'list_metrics', arguments: { filter: 'vllm' } },
    { name: 'query_metrics', arguments: { query: 'sum(vllm:num_requests_running)' } },
    {
      name: 'query_metrics',
      arguments: {
        query: 'sum(rate(vllm:generation_tokens_total[2m]))',
        mode: 'range',
        start: '-10m',
      },
    },
  ];
  calls.forEach((args, i) => {
    const res = post(
      { jsonrpc: '2.0', id: i + 2, method: 'tools/call', params: args },
      sess,
    );
    sessionNotFound.add(res.status === 404);
    const body = parseBody(res);
    const failed =
      res.status !== 200 ||
      !body ||
      !!body.error ||
      (body.result && body.result.isError === true);
    turnFailures.add(failed);
    check(res, { 'tool call 200': (r) => r.status === 200 });
  });

  // 3. Spec-compliant teardown: a stateful session is DELETEd when the
  // client is done with it - without this, every iteration leaks a session
  // until the server's SessionTimeout reaps it. Round-robin means the DELETE
  // itself can land on the wrong replica; that failure mode is already
  // measured above, so its status is not counted again here.
  const sid = sess['Mcp-Session-Id'];
  if (sid) {
    http.del(`${BASE}/mcp`, null, { headers: Object.assign({}, HDRS, sess) });
  }

  sleep(Number(__ENV.THINK_MAX_S ?? 3) * Math.random()); // agent think time; THINK_MAX_S=0 for saturation runs (H2)
}
