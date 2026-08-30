package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/crypticseeds/promscope/internal/promclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Guardrails (SPEC section 4). Constants for now; they move to PROMSCOPE_*
// config in the config pass, together with maxMetricNames.
const (
	maxLookback        = 24 * time.Hour
	maxPointsPerSeries = 200
	maxSeries          = 50
	queryTimeout       = 10 * time.Second // outer budget; upstream gets 90%
)

// QueryMetricsInput drives both query modes. Instant is the default; range
// needs start (and optionally end/step).
type QueryMetricsInput struct {
	Query string `json:"query" jsonschema:"PromQL expression e.g. rate(vllm:generation_tokens_total[5m]); discover names with list_metrics first"`
	Mode  string `json:"mode,omitempty" jsonschema:"instant (default) evaluates at now; range evaluates over start..end"`
	Start string `json:"start,omitempty" jsonschema:"range mode only: RFC3339 or negative offset from now like -1h"`
	End   string `json:"end,omitempty" jsonschema:"range mode only: RFC3339 or negative offset; defaults to now"`
	Step  string `json:"step,omitempty" jsonschema:"range mode only: duration like 30s; omit to auto-compute; coarsened if it would exceed the points budget"`
}

// QueryPoint is one sample.
type QueryPoint struct {
	T string  `json:"t" jsonschema:"RFC3339 timestamp"`
	V float64 `json:"v"`
}

// QuerySeries is one labeled series.
type QuerySeries struct {
	Labels map[string]string `json:"labels"`
	Points []QueryPoint      `json:"points"`
}

// QueryMetricsOutput is the structured result. StepUsed and the hints make
// every guardrail decision visible to the agent instead of silent.
type QueryMetricsOutput struct {
	Mode        string        `json:"mode" jsonschema:"instant or range"`
	StepUsed    string        `json:"stepUsed,omitempty" jsonschema:"the effective range step after auto-compute/coarsening"`
	Series      []QuerySeries `json:"series"`
	TotalSeries int           `json:"totalSeries" jsonschema:"series count before truncation"`
	Truncated   bool          `json:"truncated"`
	Hint        string        `json:"hint,omitempty"`
}

func (t *toolset) queryMetrics(ctx context.Context, _ *mcp.CallToolRequest, in QueryMetricsInput) (*mcp.CallToolResult, QueryMetricsOutput, error) {
	var zero QueryMetricsOutput

	if strings.TrimSpace(in.Query) == "" {
		return nil, zero, fmt.Errorf("query is required: a PromQL expression, e.g. up or rate(prometheus_http_requests_total[5m])")
	}

	// The outer budget. The upstream timeout param is 90% of it, so
	// Prometheus gives up first and we relay its clean error instead of a
	// raw context deadline (SPEC section 4).
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	upstreamTimeout := queryTimeout * 9 / 10

	now := time.Now()

	switch in.Mode {
	case "", "instant":
		if in.Start != "" || in.End != "" || in.Step != "" {
			return nil, zero, fmt.Errorf("start/end/step only apply to range queries: set mode=\"range\" or drop them")
		}
		series, warns, err := t.prom.Query(ctx, in.Query, now, upstreamTimeout)
		if err != nil {
			return nil, zero, queryToolErr(ctx, err)
		}
		out := shapeQueryOutput("instant", series, warns)
		return nil, out, nil

	case "range":
		if in.Start == "" {
			return nil, zero, fmt.Errorf("range mode requires start: RFC3339 or a negative offset like -1h")
		}
		start, err := parseTimeOrOffset(in.Start, now)
		if err != nil {
			return nil, zero, fmt.Errorf("invalid start: %v", err)
		}
		end := now
		if in.End != "" {
			if end, err = parseTimeOrOffset(in.End, now); err != nil {
				return nil, zero, fmt.Errorf("invalid end: %v", err)
			}
		}
		window := end.Sub(start)
		if window <= 0 {
			return nil, zero, fmt.Errorf("end (%s) must be after start (%s)", end.Format(time.RFC3339), start.Format(time.RFC3339))
		}
		if window > maxLookback {
			return nil, zero, fmt.Errorf("window %s exceeds the %s lookback limit: shrink the range or aggregate with a recording-rule-style query", window.Round(time.Second), maxLookback)
		}

		step, coarsened, err := chooseStep(in.Step, window)
		if err != nil {
			return nil, zero, fmt.Errorf("invalid step: %v", err)
		}

		series, warns, err := t.prom.QueryRange(ctx, in.Query, start, end, step, upstreamTimeout)
		if err != nil {
			return nil, zero, queryToolErr(ctx, err)
		}
		out := shapeQueryOutput("range", series, warns)
		out.StepUsed = step.String()
		if coarsened {
			out.Hint = joinHints(out.Hint,
				fmt.Sprintf("step coarsened to %s to keep points per series <= %d", step, maxPointsPerSeries))
		}
		return nil, out, nil

	default:
		return nil, zero, fmt.Errorf("invalid mode %q: use \"instant\" (default) or \"range\"", in.Mode)
	}
}

// queryToolErr distinguishes "our budget expired" from upstream errors,
// because the fixes differ and the agent only sees the message.
func queryToolErr(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return fmt.Errorf("query exceeded the %s budget - narrow the selector, shrink the range, or aggregate", queryTimeout)
	}
	return fmt.Errorf("query failed: %v", err)
}

// parseTimeOrOffset accepts RFC3339 ("2026-08-30T12:00:00Z") or a negative
// duration offset from now ("-1h30m"). Agents overwhelmingly want the latter.
func parseTimeOrOffset(s string, now time.Time) (time.Time, error) {
	if strings.HasPrefix(s, "-") {
		d, err := time.ParseDuration(s)
		if err != nil {
			return time.Time{}, fmt.Errorf("%q is neither RFC3339 nor a duration offset like -1h", s)
		}
		return now.Add(d), nil
	}
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither RFC3339 nor a duration offset like -1h", s)
	}
	return ts, nil
}

// chooseStep returns the effective range step: the caller's, unless it would
// exceed the points-per-series budget, in which case the minimum compliant
// step (window/maxPoints, whole seconds). Omitted step = auto-compute.
func chooseStep(requested string, window time.Duration) (step time.Duration, coarsened bool, err error) {
	minStep := time.Duration(math.Ceil(window.Seconds()/maxPointsPerSeries)) * time.Second
	if minStep < time.Second {
		minStep = time.Second
	}
	if requested == "" {
		return minStep, false, nil
	}
	req, err := time.ParseDuration(requested)
	if err != nil || req <= 0 {
		return 0, false, fmt.Errorf("%q is not a positive duration like 30s", requested)
	}
	if req < minStep {
		return minStep, true, nil
	}
	return req, false, nil
}

// shapeQueryOutput applies the series cap and drops non-finite values (JSON
// cannot carry NaN/Inf), reporting both in hints - guardrails must be
// visible, never silent.
func shapeQueryOutput(mode string, series []promclient.Series, warns []string) QueryMetricsOutput {
	out := QueryMetricsOutput{Mode: mode, TotalSeries: len(series), Series: []QuerySeries{}}

	if len(series) > maxSeries {
		series = series[:maxSeries]
		out.Truncated = true
		out.Hint = joinHints(out.Hint,
			fmt.Sprintf("%d of %d series returned - aggregate (e.g. avg by(...)) or narrow the selector", maxSeries, out.TotalSeries))
	}

	nonFinite := 0
	for _, s := range series {
		qs := QuerySeries{Labels: s.Labels, Points: make([]QueryPoint, 0, len(s.Points))}
		for _, p := range s.Points {
			if math.IsNaN(p.V) || math.IsInf(p.V, 0) {
				nonFinite++
				continue
			}
			qs.Points = append(qs.Points, QueryPoint{T: p.T.UTC().Format(time.RFC3339), V: p.V})
		}
		out.Series = append(out.Series, qs)
	}
	if nonFinite > 0 {
		out.Hint = joinHints(out.Hint,
			fmt.Sprintf("%d non-finite points (NaN/Inf) omitted - often a division by zero in the expression", nonFinite))
	}
	for _, w := range warns {
		out.Hint = joinHints(out.Hint, "prometheus warning: "+w)
	}
	return out
}

func joinHints(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}
