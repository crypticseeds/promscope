package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ListMetricsInput is the tool's argument schema. The SDK infers a JSON
// schema from this struct and validates every call against it before the
// handler runs - malformed input never reaches us. The jsonschema tag text
// becomes the property description the agent reads.
type ListMetricsInput struct {
	Filter string `json:"filter,omitempty" jsonschema:"case-insensitive substring to narrow metric names e.g. vllm"`
}

// MetricInfo is one discovered metric.
type MetricInfo struct {
	Name string `json:"name" jsonschema:"metric name to use in PromQL"`
	Type string `json:"type,omitempty" jsonschema:"counter | gauge | histogram | summary; empty when Prometheus holds no metadata for it"`
	Help string `json:"help,omitempty" jsonschema:"the metric's help text; empty when unavailable"`
}

// ListMetricsOutput is the structured result (SEP-2106 output schema).
// Truncated+Hint is the agent-ergonomics contract: when guardrails bite,
// tell the model how to ask a better question instead of silently returning less.
type ListMetricsOutput struct {
	Metrics   []MetricInfo `json:"metrics"`
	Total     int          `json:"total" jsonschema:"number of matches before truncation"`
	Truncated bool         `json:"truncated" jsonschema:"true when more matches exist than were returned"`
	Hint      string       `json:"hint,omitempty" jsonschema:"how to get a better result; present when truncated or degraded"`
}

func (t *toolset) listMetrics(ctx context.Context, _ *mcp.CallToolRequest, in ListMetricsInput) (*mcp.CallToolResult, ListMetricsOutput, error) {
	var zero ListMetricsOutput

	names, err := t.prom.MetricNames(ctx)
	if err != nil {
		// A returned error becomes a tool result with isError=true, not a
		// protocol error. Write it for the agent: name the failure and the
		// likely fix.
		return nil, zero, fmt.Errorf("listing metric names failed: %v - check that Prometheus is reachable (PROMSCOPE_PROMETHEUS_URL)", err)
	}

	// Filter first, then truncate: Total must mean "all matches", never
	// "all metrics", or the truncation hint would lie.
	matches := names
	if in.Filter != "" {
		f := strings.ToLower(in.Filter)
		matches = nil
		for _, n := range names {
			if strings.Contains(strings.ToLower(n), f) {
				matches = append(matches, n)
			}
		}
	}

	out := ListMetricsOutput{Total: len(matches)}
	var hints []string

	if len(matches) == 0 && in.Filter != "" {
		hints = append(hints, fmt.Sprintf("no metric names contain %q - try a broader filter or omit it", in.Filter))
	}
	if len(matches) > maxMetricNames {
		matches = matches[:maxMetricNames]
		out.Truncated = true
		hints = append(hints, fmt.Sprintf("showing %d of %d matches - pass a narrower filter", maxMetricNames, out.Total))
	}

	// Metadata enrichment (D6) degrades gracefully: names alone are still
	// useful, so a metadata failure is a note in the hint, not an error.
	meta, mdErr := t.prom.Metadata(ctx)
	if mdErr != nil {
		hints = append(hints, "metric type/help omitted: metadata endpoint failed")
	}

	out.Metrics = make([]MetricInfo, len(matches))
	for i, n := range matches {
		mi := MetricInfo{Name: n}
		if m, ok := meta[n]; ok {
			mi.Type, mi.Help = m.Type, m.Help
		}
		out.Metrics[i] = mi
	}
	out.Hint = strings.Join(hints, "; ")

	return nil, out, nil
}
