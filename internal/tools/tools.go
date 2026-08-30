// Package tools implements promscope's MCP tool surface: three read-only
// tools over a promclient.Client. The surface is frozen (SPEC section 6).
package tools

import (
	"github.com/crypticseeds/promscope/internal/promclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxMetricNames caps list_metrics output (SPEC section 4, guardrails).
// LLM context windows are the scarce resource; past this point the fix is a
// narrower filter, not more output.
const maxMetricNames = 500

// toolset carries the shared dependencies of all tool handlers.
type toolset struct {
	prom promclient.Client
}

// Register wires every promscope tool onto server. It is this package's only
// entry point: main stays ignorant of individual tools.
func Register(server *mcp.Server, prom promclient.Client) {
	ts := &toolset{prom: prom}

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
}
