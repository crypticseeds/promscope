# promscope load tests

Three designed experiments, not random ramping. Each exists to confirm or
kill a stated hypothesis about what MCP's stateless mode actually buys.
Workloads run as dockerized k6 on the compose network - nothing to install:

```bash
docker run --rm -i --network promscope_default \
  -e VUS=10 -e DURATION=60s grafana/k6 run - < loadtest/k6/agent-turn.js
```

## Methodology, and its honest limits

- **Workload model**: `agent-turn.js` is one agent interaction - initialize,
  discover, instant query, range query, think time (`THINK_MAX_S=0` for
  saturation runs). It is a spec-compliant client: echoes the negotiated
  `MCP-Protocol-Version`, carries `Mcp-Session-Id` when issued, DELETEs its
  session at turn end. `session-churn.js` deliberately does NOT delete -
  it exists to measure retention (H3).
- **Closed-loop** (constant VUs): under-reports latency spikes (coordinated
  omission). Fine here - we compare configurations against each other, not
  publish absolute benchmarks.
- **One laptop, one shared Prometheus**: all numbers are relative. The
  negative `min` k6 sometimes reports is container clock-skew noise.
- Fixed PromQL across every run so Prometheus is a controlled variable.
- Flip the whole stack's mode with one variable (config homogeneity by
  construction): `PROMSCOPE_STATELESS=false docker compose -f deploy/compose.yaml up -d`

## H1 - Correctness: a stateful server misbehaves behind round-robin

**Setup:** 2 replicas, nginx round-robin, identical `agent-turn.js`
(10 VUs, 60s); only `PROMSCOPE_STATELESS` differs.

**Read this table correctly: promscope ships stateless and produced ZERO
failures.** The 49.33% column is the *rejected alternative* - stateful mode
exists only behind a test flag so its failure mode could be measured. It is
the configuration promscope exists to avoid.

| Metric | promscope as shipped (stateless) | Rejected alternative (stateful flag) |
|---|---|---|
| `session_not_found` (404s) | **0.00%** | 49.33% |
| `agent_turn_failures` | **0.00%** | 34.66% |
| p95 latency | 13.1ms | 9.7ms |

Theoretical failure rate for stateful mode behind 2-replica round-robin is
50%; measured 49.33%. The stateful run's *better* p95 is the trap worth
naming: failing fast with a 404 is cheaper than querying Prometheus. Never
read latency without its error rate.

## H2 - Scaling: does a second replica buy throughput?

**Setup:** stateless, saturation profile (30 VUs, `THINK_MAX_S=0`, 90s),
single upstream (`compose.single.yaml`) vs both replicas.

| Config | Throughput | p95 | Failures |
|---|---|---|---|
| 1 replica | 315.3 req/s | 198.5ms | 0% |
| 2 replicas | 329.5 req/s (+4.5%) | 239.6ms | 0% |

**Finding, reported honestly:** near-zero scaling - because promscope
(~3ms/request) was never the bottleneck; the shared single Prometheus and
the host were. Horizontal scaling helps only when the scaled tier is the
constraint. What the second replica *does* buy at zero correctness cost is
exactly H1's result: any replica serves any request. Scaling the stateless
tier is safe; whether it is useful depends on where your bottleneck lives.

## H3 - Memory: session churn under retention vs statelessness

**Setup:** `session-churn.js` (20 VUs, 120s, initialize + ping, no DELETE),
`scripts/memwatch.sh` sampling container RSS every 2s. Stateful mode runs
with `SessionTimeout: 30m`, so nothing is reaped inside the window.

| Mode | Sessions created | Replica A RSS | Replica B RSS |
|---|---|---|---|
| Rejected alternative (stateful flag) | 98,433 | 8.1 -> 802.2 MiB | 14.0 -> 800.2 MiB |
| **promscope as shipped (stateless)** | 74,911 iterations | **7.4 -> 8.8 MiB** | **5.3 -> 8.6 MiB** |

~8 KiB retained per stranded session, growing linearly: two minutes of
unauthenticated traffic held ~1.6 GB across the pair. Stateless stayed flat
to within GC noise. This is the security review's "memory-exhaustion path"
with a number on it - and why `SessionTimeout` is mandatory whenever the
stateful A/B toggle is used.

Raw data: `results/h2-*.json` (k6 summary exports), `results/h3-*-mem.csv`
(RSS time series).

## Reproducing

```bash
# H1 - run twice, flipping PROMSCOPE_STATELESS
docker run --rm -i --network promscope_default -e VUS=10 -e DURATION=60s \
  grafana/k6 run - < loadtest/k6/agent-turn.js

# H2 - single upstream, then both
docker compose -f deploy/compose.yaml -f deploy/compose.single.yaml up -d nginx
docker run --rm -i --network promscope_default -e VUS=30 -e DURATION=90s -e THINK_MAX_S=0 \
  grafana/k6 run - < loadtest/k6/agent-turn.js
docker compose -f deploy/compose.yaml up -d nginx   # restore

# H3 - churn with the sampler running
./loadtest/scripts/memwatch.sh loadtest/results/mem.csv &
docker run --rm -i --network promscope_default -e VUS=20 -e DURATION=120s \
  grafana/k6 run - < loadtest/k6/session-churn.js
kill %1
```

Longer runs: raise `DURATION`, repeat 3x, take medians. The numbers above
are single 60-120s runs - directionally decisive (0% vs 49%, flat vs 100x),
which is what these hypotheses needed.
