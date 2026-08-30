// Package tools implements promscope's MCP tool surface: three read-only
// tools over a promclient.Client. The surface is frozen (SPEC section 6).
package tools

import (
	"fmt"
	"time"

	"github.com/crypticseeds/promscope/internal/promclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Limits are the server-side guardrails (SPEC section 4). They exist to
// protect the agent's context window and the upstream Prometheus - never
// trusted to the model, always reported when they bite.
type Limits struct {
	MaxLookback        time.Duration // widest allowed range-query window
	MaxSeries          int           // series returned per query before truncation
	MaxPointsPerSeries int           // drives range-step auto-compute/coarsening
	QueryTimeout       time.Duration // outer per-call budget; upstream gets 90%
	MaxMetricNames     int           // list_metrics cap before truncation
}

// DefaultLimits are the documented, tested defaults.
func DefaultLimits() Limits {
	return Limits{
		MaxLookback:        24 * time.Hour,
		MaxSeries:          50,
		MaxPointsPerSeries: 200,
		QueryTimeout:       10 * time.Second,
		MaxMetricNames:     500,
	}
}

// Validate rejects nonsense before it can corrupt guardrail math.
func (l Limits) Validate() error {
	if l.MaxLookback <= 0 || l.MaxSeries <= 0 || l.MaxMetricNames <= 0 {
		return fmt.Errorf("all limits must be positive: %+v", l)
	}
	if l.MaxPointsPerSeries < 2 {
		return fmt.Errorf("max points per series must be at least 2: step auto-compute divides by maxPoints-1 (Prometheus returns floor(window/step)+1 samples)")
	}
	if l.QueryTimeout < time.Second {
		return fmt.Errorf("query timeout %s is below the 1s floor", l.QueryTimeout)
	}
	return nil
}

// toolset carries the shared dependencies of all tool handlers.
type toolset struct {
	prom   promclient.Client
	limits Limits
}

// Register wires every promscope tool onto server. It is this package's only
// entry point: main stays ignorant of individual tools. Limits must have
// been validated by the caller.
func Register(server *mcp.Server, prom promclient.Client, limits Limits) {
	ts := &toolset{prom: prom, limits: limits}

	mcp.AddTool(server, &mcp.Tool{
		Name: "list_metrics",
		Description: "Discover Prometheus metric names with type and help text where available. " +
			"Call this before query_metrics: you cannot write PromQL for metrics you have not seen. " +
			"Use filter to narrow results (case-insensitive substring) e.g. filter=\"vllm\" for inference metrics.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
		},
	}, ts.listMetrics)

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_alerts",
		Description: "List active Prometheus alerts (firing and pending). " +
			"Returns each alert's labels, annotations, active-since time and last value. " +
			"Optionally pass state=\"firing\" or state=\"pending\" to narrow.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
		},
	}, ts.getAlerts)

	mcp.AddTool(server, &mcp.Tool{
		Name: "query_metrics",
		Description: "Evaluate a PromQL expression against Prometheus. Default mode is an instant query at now; " +
			"set mode=\"range\" with start (e.g. -1h) for a time series. " +
			"Prefer aggregations (rate(), avg by(), histogram_quantile()) over raw selectors: results are capped at " +
			"50 series and 200 points per series, and truncation is reported in the hint.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
		},
	}, ts.queryMetrics)

	server.AddResource(&mcp.Resource{
		URI:         rulesURI,
		Name:        "rules",
		Title:       "Prometheus alerting and recording rules",
		Description: "The configured rule groups as JSON. Read this to see what get_alerts can fire and which recording rules exist.",
		MIMEType:    "application/json",
	}, ts.readRules)
}
