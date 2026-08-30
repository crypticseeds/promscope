package tools

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/crypticseeds/promscope/internal/promclient"
)

func sampleSeries(n int) []promclient.Series {
	out := make([]promclient.Series, n)
	for i := range out {
		out[i] = promclient.Series{
			Labels: map[string]string{"job": fmt.Sprintf("job-%03d", i)},
			Points: []promclient.Point{{T: time.Unix(1724800000, 0), V: float64(i)}},
		}
	}
	return out
}

func TestQueryMetricsValidation(t *testing.T) {
	tests := []struct {
		name    string
		in      QueryMetricsInput
		wantErr string // substring the error must contain
	}{
		{"empty query", QueryMetricsInput{}, "query is required"},
		{"invalid mode", QueryMetricsInput{Query: "up", Mode: "streaming"}, `"instant" (default) or "range"`},
		{"range fields on instant", QueryMetricsInput{Query: "up", Start: "-1h"}, "mode=\"range\""},
		{"range without start", QueryMetricsInput{Query: "up", Mode: "range"}, "requires start"},
		{"bad start", QueryMetricsInput{Query: "up", Mode: "range", Start: "yesterday"}, "neither RFC3339 nor a duration"},
		{"bad end", QueryMetricsInput{Query: "up", Mode: "range", Start: "-1h", End: "later"}, "invalid end"},
		{"end before start", QueryMetricsInput{Query: "up", Mode: "range", Start: "-1h", End: "-2h"}, "must be after start"},
		{"window over lookback", QueryMetricsInput{Query: "up", Mode: "range", Start: "-25h"}, "lookback limit"},
		{"bad step", QueryMetricsInput{Query: "up", Mode: "range", Start: "-1h", Step: "fast"}, "not a positive duration"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestToolset(&fakeClient{querySeries: sampleSeries(1)})
			_, _, err := ts.queryMetrics(context.Background(), nil, tt.in)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestQueryMetricsInstant(t *testing.T) {
	fake := &fakeClient{querySeries: sampleSeries(2)}
	ts := newTestToolset(fake)

	_, out, err := ts.queryMetrics(context.Background(), nil, QueryMetricsInput{Query: "up"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Mode != "instant" || out.TotalSeries != 2 || out.Truncated {
		t.Errorf("unexpected shape: %+v", out)
	}
	if fake.lastQuery != "up" {
		t.Errorf("query passed upstream = %q, want up", fake.lastQuery)
	}
	if fake.lastTimeout != 9*time.Second {
		t.Errorf("upstream timeout = %v, want 9s (90%% of the 10s budget)", fake.lastTimeout)
	}
	if out.Series[0].Points[0].T == "" || !strings.HasSuffix(out.Series[0].Points[0].T, "Z") {
		t.Errorf("points should carry RFC3339 UTC timestamps, got %q", out.Series[0].Points[0].T)
	}
}

func TestQueryMetricsStepAutoCompute(t *testing.T) {
	fake := &fakeClient{querySeries: sampleSeries(1)}
	ts := newTestToolset(fake)

	// 10h window, 200-point budget: ceil(36000s / 199) = 181s. The divisor
	// is maxPoints-1 because Prometheus returns floor(window/step)+1 points.
	_, out, err := ts.queryMetrics(context.Background(), nil,
		QueryMetricsInput{Query: "up", Mode: "range", Start: "-10h"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.lastStep != 181*time.Second {
		t.Errorf("auto step = %v, want 181s (ceil(10h / 199))", fake.lastStep)
	}
	if out.StepUsed != "3m1s" {
		t.Errorf("StepUsed = %q, want 3m1s", out.StepUsed)
	}
	if strings.Contains(out.Hint, "coarsened") {
		t.Errorf("auto-computed step is not coarsening, hint = %q", out.Hint)
	}
}

func TestQueryMetricsStepCoarsened(t *testing.T) {
	fake := &fakeClient{querySeries: sampleSeries(1)}
	ts := newTestToolset(fake)

	// 1s step over 10h would be 36000 points/series; must coarsen to 181s
	// and say so.
	_, out, err := ts.queryMetrics(context.Background(), nil,
		QueryMetricsInput{Query: "up", Mode: "range", Start: "-10h", Step: "1s"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.lastStep != 181*time.Second {
		t.Errorf("coarsened step = %v, want 181s", fake.lastStep)
	}
	if !strings.Contains(out.Hint, "coarsened") {
		t.Errorf("coarsening must be reported, hint = %q", out.Hint)
	}

	// A generous step is respected as-is.
	_, out, err = ts.queryMetrics(context.Background(), nil,
		QueryMetricsInput{Query: "up", Mode: "range", Start: "-10h", Step: "10m"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.lastStep != 10*time.Minute || strings.Contains(out.Hint, "coarsened") {
		t.Errorf("valid step should pass through, got step=%v hint=%q", fake.lastStep, out.Hint)
	}
}

func TestQueryMetricsSeriesTruncation(t *testing.T) {
	lim := DefaultLimits()
	ts := newTestToolset(&fakeClient{querySeries: sampleSeries(lim.MaxSeries + 10)})

	_, out, err := ts.queryMetrics(context.Background(), nil, QueryMetricsInput{Query: "up"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Truncated || len(out.Series) != lim.MaxSeries || out.TotalSeries != lim.MaxSeries+10 {
		t.Errorf("truncation wrong: truncated=%v len=%d total=%d", out.Truncated, len(out.Series), out.TotalSeries)
	}
	if !strings.Contains(out.Hint, "aggregate") {
		t.Errorf("truncation hint should suggest aggregation, got %q", out.Hint)
	}
}

func TestQueryMetricsNonFiniteDropped(t *testing.T) {
	series := []promclient.Series{{
		Labels: map[string]string{"job": "x"},
		Points: []promclient.Point{
			{T: time.Unix(1724800000, 0), V: 1},
			{T: time.Unix(1724800015, 0), V: math.NaN()},
			{T: time.Unix(1724800030, 0), V: math.Inf(1)},
			{T: time.Unix(1724800045, 0), V: 2},
		},
	}}
	ts := newTestToolset(&fakeClient{querySeries: series})

	_, out, err := ts.queryMetrics(context.Background(), nil, QueryMetricsInput{Query: "x/y"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Series[0].Points) != 2 {
		t.Errorf("non-finite points must be dropped, got %+v", out.Series[0].Points)
	}
	if !strings.Contains(out.Hint, "2 non-finite") {
		t.Errorf("dropped points must be counted in the hint, got %q", out.Hint)
	}
}

func TestQueryMetricsWarningsSurface(t *testing.T) {
	ts := newTestToolset(&fakeClient{
		querySeries: sampleSeries(1),
		queryWarns:  []string{"query would load too many samples"},
	})

	_, out, err := ts.queryMetrics(context.Background(), nil, QueryMetricsInput{Query: "up"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.Hint, "prometheus warning") {
		t.Errorf("warnings must surface in the hint, got %q", out.Hint)
	}
}

func TestQueryMetricsEmptyResultHint(t *testing.T) {
	ts := newTestToolset(&fakeClient{querySeries: nil})

	_, out, err := ts.queryMetrics(context.Background(), nil, QueryMetricsInput{Query: "no_such_metric"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.TotalSeries != 0 || len(out.Series) != 0 {
		t.Fatalf("expected empty result, got %+v", out)
	}
	if !strings.Contains(out.Hint, "list_metrics") {
		t.Errorf("empty result must point at discovery, hint = %q", out.Hint)
	}
}

func TestQueryMetricsFutureEndClamped(t *testing.T) {
	fake := &fakeClient{querySeries: sampleSeries(1)}
	ts := newTestToolset(fake)

	futureEnd := time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	_, out, err := ts.queryMetrics(context.Background(), nil,
		QueryMetricsInput{Query: "up", Mode: "range", Start: "-30m", End: futureEnd})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.lastEnd.After(time.Now().Add(time.Minute)) {
		t.Errorf("end was not clamped: %v went upstream", fake.lastEnd)
	}
	if !strings.Contains(out.Hint, "clamped") {
		t.Errorf("clamping must be reported, hint = %q", out.Hint)
	}
}

func TestQueryMetricsUpstreamError(t *testing.T) {
	ts := newTestToolset(&fakeClient{queryErr: fmt.Errorf(`parse error: unexpected ")"`)})

	_, _, err := ts.queryMetrics(context.Background(), nil, QueryMetricsInput{Query: "up)"})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "parse error") {
		t.Errorf("upstream detail must survive: %v", err)
	}
}
