package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestBudgetAppliesToEveryHandler pins the review fix (Sol M3 / Fable
// MAJ-1): a stalled Prometheus must not hang list_metrics (or any handler)
// past the configured budget - previously only query_metrics was guarded.
func TestBudgetAppliesToEveryHandler(t *testing.T) {
	lim := DefaultLimits()
	lim.QueryTimeout = 100 * time.Millisecond // bypasses Validate on purpose
	ts := &toolset{
		prom:   &fakeClient{block: make(chan struct{})}, // never closes: hung upstream
		limits: lim,
		sem:    make(chan struct{}, lim.MaxInflight),
	}

	start := time.Now()
	_, _, err := ts.listMetrics(context.Background(), nil, ListMetricsInput{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want error from a hung upstream, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("budget did not bound the handler: took %v", elapsed)
	}
}

// TestUpstreamConcurrencyLimit pins the semaphore (Sol M2): when every slot
// is held, further calls fail with a retryable capacity error instead of
// piling onto Prometheus.
func TestUpstreamConcurrencyLimit(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxInflight = 1
	lim.QueryTimeout = 100 * time.Millisecond
	ts := &toolset{prom: &fakeClient{names: []string{"up"}}, limits: lim, sem: make(chan struct{}, lim.MaxInflight)}

	ts.sem <- struct{}{} // occupy the only slot
	defer func() { <-ts.sem }()

	_, _, err := ts.listMetrics(context.Background(), nil, ListMetricsInput{})
	if err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("want retryable capacity error, got: %v", err)
	}
}

// TestQueryToolErrTaxonomy pins the cancellation-vs-deadline distinction
// (Fable MIN-4 / CodeRabbit): the two causes have different fixes and the
// message is the agent's only interface.
func TestQueryToolErrTaxonomy(t *testing.T) {
	tests := []struct {
		name    string
		errIn   error
		want    string
		wantNot string
	}{
		{"deadline names the budget", context.DeadlineExceeded, "budget", "canceled"},
		{"cancellation is not blamed on the budget", context.Canceled, "canceled", "budget"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestToolset(&fakeClient{queryErr: tt.errIn})
			_, _, err := ts.queryMetrics(context.Background(), nil, QueryMetricsInput{Query: "up"})
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), tt.wantNot) {
				t.Fatalf("error %q: want %q and not %q", err, tt.want, tt.wantNot)
			}
		})
	}
}

// TestQueryLengthCap pins the sanity bound on the PromQL string (Sol M2).
func TestQueryLengthCap(t *testing.T) {
	ts := newTestToolset(&fakeClient{querySeries: sampleSeries(1)})
	_, _, err := ts.queryMetrics(context.Background(), nil,
		QueryMetricsInput{Query: strings.Repeat("x", maxQueryChars+1)})
	if err == nil || !strings.Contains(err.Error(), "4096") {
		t.Fatalf("want query-length error naming the limit, got: %v", err)
	}
}
