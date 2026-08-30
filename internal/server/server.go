// Package server assembles promscope's HTTP surface: the MCP endpoint plus
// the operational endpoints (/healthz, /metrics) that make the observability
// tool itself observable (SPEC section 4).
package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/crypticseeds/promscope/internal/promclient"
)

// Server owns the self-observability state: a private Prometheus registry
// (never the package-global default - tests and multiple instances stay
// isolated) and the promscope_* metric families.
type Server struct {
	registry *prometheus.Registry
	httpReqs *prometheus.CounterVec
	httpDur  *prometheus.HistogramVec
	upstream *prometheus.CounterVec
	version  string
}

// New builds the observability state. Metric naming follows the
// service-prefixed snake_case convention (promscope_*).
func New(version string) *Server {
	reg := prometheus.NewRegistry()
	s := &Server{
		registry: reg,
		version:  version,
		httpReqs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "promscope_http_requests_total",
			Help: "MCP endpoint requests by HTTP status code.",
		}, []string{"code"}),
		httpDur: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "promscope_http_request_duration_seconds",
			Help:    "MCP endpoint request duration.",
			Buckets: prometheus.DefBuckets,
		}, []string{"code"}),
		upstream: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "promscope_upstream_requests_total",
			Help: "Prometheus API calls by operation and outcome.",
		}, []string{"op", "outcome"}),
	}
	reg.MustRegister(
		s.httpReqs, s.httpDur, s.upstream,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return s
}

// Handler returns the full HTTP surface: the (instrumented) MCP handler on
// /mcp, liveness on /healthz, and the metrics exposition on /metrics.
func (s *Server) Handler(mcpHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", promhttp.InstrumentHandlerCounter(s.httpReqs,
		promhttp.InstrumentHandlerDuration(s.httpDur, mcpHandler)))
	mux.HandleFunc("/healthz", s.healthz)
	mux.Handle("/metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))
	return mux
}

// healthz is liveness only: 200 whenever the process serves. It deliberately
// does NOT probe Prometheus - coupling liveness to a dependency turns an
// upstream outage into a restart storm of the wrong service.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","version":%q}`+"\n", s.version)
}

// WrapClient decorates a promclient.Client so every upstream call is counted
// by operation and outcome. Because tools depend on the interface, the
// decorator slots in with zero changes to promclient or the handlers - the
// same seam the fakes use in tests.
func (s *Server) WrapClient(c promclient.Client) promclient.Client {
	return &instrumentedClient{next: c, upstream: s.upstream}
}

type instrumentedClient struct {
	next     promclient.Client
	upstream *prometheus.CounterVec
}

var _ promclient.Client = (*instrumentedClient)(nil)

func (ic *instrumentedClient) count(op string, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	ic.upstream.WithLabelValues(op, outcome).Inc()
}

func (ic *instrumentedClient) MetricNames(ctx context.Context) ([]string, error) {
	v, err := ic.next.MetricNames(ctx)
	ic.count("metric_names", err)
	return v, err
}

func (ic *instrumentedClient) Metadata(ctx context.Context) (map[string]promclient.Meta, error) {
	v, err := ic.next.Metadata(ctx)
	ic.count("metadata", err)
	return v, err
}

func (ic *instrumentedClient) Alerts(ctx context.Context) ([]promclient.Alert, error) {
	v, err := ic.next.Alerts(ctx)
	ic.count("alerts", err)
	return v, err
}

func (ic *instrumentedClient) Rules(ctx context.Context) ([]promclient.RuleGroup, error) {
	v, err := ic.next.Rules(ctx)
	ic.count("rules", err)
	return v, err
}

func (ic *instrumentedClient) Query(ctx context.Context, promql string, ts time.Time, timeout time.Duration) ([]promclient.Series, []string, error) {
	v, w, err := ic.next.Query(ctx, promql, ts, timeout)
	ic.count("query", err)
	return v, w, err
}

func (ic *instrumentedClient) QueryRange(ctx context.Context, promql string, start, end time.Time, step, timeout time.Duration) ([]promclient.Series, []string, error) {
	v, w, err := ic.next.QueryRange(ctx, promql, start, end, step, timeout)
	ic.count("query_range", err)
	return v, w, err
}
