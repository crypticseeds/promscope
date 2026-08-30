# promscope - Specification & Architecture

**Status:** Draft v1 (locked scope)
**Last updated:** 2026-08-26

---

## 1. What we are building

**promscope** is a **stateless MCP (Model Context Protocol) server**, written in Go with the
official [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk), that
gives AI agents read-only access to Prometheus - framed around **LLM inference
observability** (vLLM natively exports Prometheus metrics: tokens/sec, TTFT, KV-cache usage).

The tools are deliberately boring. The project's value is the **infrastructure story**:

1. **Statelessness.** The server runs in the SDK's stateless streamable-HTTP mode
   (no `Mcp-Session-Id` validation, no server-side session state). Prometheus's HTTP API is
   itself stateless, so the stack is honestly stateless end-to-end - any replica can serve
   any request, no sticky sessions, trivially horizontally scalable.
2. **Spec fluency.** We use MCP features most servers skip: structured tool output schemas,
   tool annotations, and a resource - not just bare tools returning strings.
3. **Operational craft.** Context propagation, timeouts, graceful shutdown, proper tool
   errors, table-driven tests, and a server that exports its own `/metrics`.

## 2. Why (motivation)

- **Resume/portfolio signal for AI infra roles.** Kubernetes read-only MCP servers are
  commodity; an inference-observability MCP server that demonstrates PromQL fluency and
  production-minded Go is distinctive.
- **Blog material.** The MCP spec is moving toward a sessionless revision. Building on
  stateless mode now and documenting *which MCP features survive statelessness* (and which
  break - subscriptions, server-initiated notifications) is a genuinely useful article.
- **Learning goal.** Hands-on practice with the official Go SDK, the streamable HTTP
  transport, and the difference between stateful and stateless operation - including a
  load-test comparison across replicas.

## 3. What it does

An MCP client (Claude Desktop, an agent framework, `mcp-inspector`, or plain curl) connects
over streamable HTTP and can:

| Capability | Kind | Description |
|---|---|---|
| `list_metrics` | tool | Metric name + label discovery (with optional filter), enriched with each metric's type and help text from `/api/v1/metadata`. An LLM cannot write PromQL for metrics it cannot see - discovery-first is deliberate agent ergonomics. |
| `query_metrics` | tool | Execute a PromQL instant or range query, with guardrails: max lookback window, server-side timeout, result size cap. |
| `get_alerts` | tool | List firing/pending alerts from Prometheus. |
| `prometheus://rules` | resource | The alert/recording rule configuration, exposed as an MCP resource - demonstrates MCP is more than tools. |

That is the complete surface. **No fourth tool.**

## 4. Architecture

```
 MCP client (agent / inspector / curl)
        |  streamable HTTP (POST /mcp), no session state
        v
 load balancer (round-robin, NO sticky sessions)
        |
        v
 promscope replica x N        <- stateless: any replica serves any request
        |  net/http client: context propagation, timeout, size cap
        v
 Prometheus HTTP API (/api/v1/*)
        ^
        |  scrapes
 exporters: node-exporter / mock inference exporter (vLLM-shaped metrics)
```

### Components

- **`cmd/promscope`** - main: flag/env config, HTTP server wiring, graceful shutdown.
- **MCP layer** - `mcp.Server` + `StreamableHTTPHandler` with `Stateless: true`.
  Tools registered with input *and output* JSON schemas and annotations
  (`readOnlyHint: true`, `idempotentHint: true`).
- **Prometheus client** - the official `client_golang/api/prometheus/v1` package (typed,
  battle-tested parsing of Matrix/Vector/Scalar; covers every endpoint we need), wrapped
  in a thin local interface for testability. Decision D2: the boring standard choice -
  our innovation budget belongs to MCP, not JSON parsing.
- **Guardrails** - enforced server-side, not trusted to the model:
  - range queries capped at a max lookback window (config, default 24h)
  - step auto-computed so points per series ≤ the budget (config, default
    200; fencepost-exact - Prometheus returns floor(window/step)+1 samples);
    max series per result (config, default 50; truncate + `truncated: true`
    \+ hint to aggregate); future range ends clamped to now and reported
  - per-request timeout budget (config, default 10s) via `context.WithTimeout`;
    Prometheus's own `timeout` query param set to 90% of it so upstream quits first
  - upstream response body capped (config, default 1 MiB) at the HTTP
    transport, reported with the configured size and the fix
- **Self-observability** - `/healthz` endpoint and `/metrics` (Prometheus format) on the
  promscope server itself: request counts, latencies, upstream errors.

### Error handling policy

- Bad input / upstream failure -> **tool result with `isError: true`** and an actionable
  message the model can act on (e.g. "unknown metric; call list_metrics first").
  Protocol-level errors are reserved for protocol-level problems.
- All upstream calls honor request context: client cancellation propagates to Prometheus.

## 5. Statelessness: the design constraint

In the go-sdk, `StreamableHTTPHandler{Stateless: true}` means:

- no session is created or validated; every POST is self-contained
- no server-side state may be assumed between calls - each tool handler is a pure function
  of (request, config, Prometheus)
- consequences we will document (blog core): standalone SSE streams, resource
  subscriptions, and server-initiated notifications (`listChanged`) do not work without a
  session; request-scoped streaming still does

**Rule:** nothing in the codebase may hold per-client state. If a feature needs it, the
feature is out of scope.

**Corollary - config homogeneity:** "any replica serves any request" additionally
assumes every replica runs *identical configuration*. Guardrail limits differing
across replicas make the same request behave differently behind round-robin, which
presents as flakiness. The M4 compose stack therefore uses a single shared env
block for all promscope replicas.

## 6. Non-goals (accepted limitations - written down to prevent scope creep)

- **No auth/OAuth.** Bearer-token env var pass-through at most. Accepted limitation, noted
  in README.
- **No write operations.** Read-only, always.
- **No dynamic tool registration**, no prompts, no sampling, no elicitation.
- **No Kubernetes integration.**
- **No caching** (D3) and **no automatic retries** (D5). The calling agent is the retry
  loop; a cache adds staleness/invalidation semantics and muddies load-test numbers.
- **No Prometheus client library abstractions** beyond what the three tools need.
- Tool count is frozen at three.

## 7. Milestones

1. **Skeleton** - module init, stateless `StreamableHTTPHandler` answering `initialize`;
   verified with curl. No tools yet.
2. **Tools** - `list_metrics`, `query_metrics`, `get_alerts` with structured output,
   annotations, guardrails; table-driven tests against `httptest` mock Prometheus.
3. **Resource** - `prometheus://rules`.
4. **Ops** - `/healthz`, `/metrics`, graceful shutdown, Dockerfile, docker-compose demo
   stack (Prometheus + node-exporter + mock vLLM exporter + 2 promscope replicas + nginx).
   Documented optional scrape recipes: real vLLM on RunPod (env-templated pod URL,
   Doppler-rendered auth if `/metrics` is key-protected) and the sre-inference-gateway
   (`gateway_*` golden signals) - both are config-only, zero promscope code.
5. **Proof** - k6/vegeta load test: stateful vs stateless mode across round-robin
   replicas; capture numbers for README + blog.

## 8. Verification

- `go test ./...` green at every milestone; handlers tested against mock Prometheus.
- Manual: MCP Inspector and curl against a real docker-compose Prometheus.
- Milestone 5 is the ultimate proof: a *stateful* server misbehaves behind round-robin
  (unknown session IDs), the stateless one does not - demonstrated with numbers.

## 9. Decisions log (2026-08-27, architecture grilling v1)

| # | Decision | Choice |
|---|---|---|
| D1 | Demo data | Mock vLLM exporter (local, no GPU) **and** documented real-vLLM scrape recipe (RunPod). promscope is metric-agnostic - real vs mock is purely a Prometheus scrape-config concern. |
| D2 | Prometheus client | Official `client_golang/api/prometheus/v1`, wrapped in a thin interface. |
| D3 | Caching | None in v1. |
| D4 | Load test tool | k6. |
| D5 | Retries | None - the calling agent is the retry loop. |
| D6 | list_metrics | Enriched with type + help from `/api/v1/metadata`. |
| D7 | Name | promscope (locked before `go mod init`). |
| - | Default port | `:8090` - avoids collisions with sre-inference-gateway's map (gateway 8000, vLLM 8080, Prometheus 9091, Grafana 3000). `PROMSCOPE_PROMETHEUS_URL=http://localhost:9091` points promscope at the existing gateway stack's Prometheus with no compose changes. |
| - | Combined demo | Optional recipe: llm-slo-bench load at the gateway, agent diagnoses the p99 TTFT ≤ 250ms SLO breach via promscope (circuit breaker state, TTFT percentiles, shed requests). Config/docs only. |
