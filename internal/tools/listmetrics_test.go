package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/crypticseeds/promscope/internal/promclient"
)

// fakeClient satisfies promclient.Client with canned data - the payoff of
// tools depending on the interface: no HTTP, no mocks framework, 10 lines.
type fakeClient struct {
	names    []string
	namesErr error
	meta     map[string]promclient.Meta
	metaErr  error
}

func (f *fakeClient) MetricNames(context.Context) ([]string, error) {
	return f.names, f.namesErr
}

func (f *fakeClient) Metadata(context.Context) (map[string]promclient.Meta, error) {
	return f.meta, f.metaErr
}

func TestListMetrics(t *testing.T) {
	baseNames := []string{"go_goroutines", "up", "vllm:gpu_cache_usage_perc", "vllm:num_requests_running"}
	baseMeta := map[string]promclient.Meta{
		"up": {Type: "gauge", Help: "1 if the scrape succeeded"},
	}

	tests := []struct {
		name      string
		client    *fakeClient
		in        ListMetricsInput
		wantNames []string
		wantTotal int
		wantTrunc bool
		wantHint  string // substring the hint must contain; "" means no requirement
	}{
		{
			name:      "no filter returns everything enriched",
			client:    &fakeClient{names: baseNames, meta: baseMeta},
			wantNames: baseNames,
			wantTotal: 4,
		},
		{
			name:      "filter is a case-insensitive substring",
			client:    &fakeClient{names: baseNames, meta: baseMeta},
			in:        ListMetricsInput{Filter: "VLLM"},
			wantNames: []string{"vllm:gpu_cache_usage_perc", "vllm:num_requests_running"},
			wantTotal: 2,
		},
		{
			name:      "no matches hints at a broader filter",
			client:    &fakeClient{names: baseNames, meta: baseMeta},
			in:        ListMetricsInput{Filter: "nomatch"},
			wantNames: []string{},
			wantTotal: 0,
			wantHint:  "broader filter",
		},
		{
			name:      "metadata failure degrades instead of erroring",
			client:    &fakeClient{names: baseNames, metaErr: fmt.Errorf("boom")},
			wantNames: baseNames,
			wantTotal: 4,
			wantHint:  "metadata endpoint failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := &toolset{prom: tt.client}
			_, out, err := ts.listMetrics(context.Background(), nil, tt.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out.Total != tt.wantTotal {
				t.Errorf("Total = %d, want %d", out.Total, tt.wantTotal)
			}
			if out.Truncated != tt.wantTrunc {
				t.Errorf("Truncated = %v, want %v", out.Truncated, tt.wantTrunc)
			}
			if len(out.Metrics) != len(tt.wantNames) {
				t.Fatalf("got %d metrics, want %d (%v)", len(out.Metrics), len(tt.wantNames), out.Metrics)
			}
			for i, want := range tt.wantNames {
				if out.Metrics[i].Name != want {
					t.Errorf("Metrics[%d].Name = %q, want %q", i, out.Metrics[i].Name, want)
				}
			}
			if tt.wantHint != "" && !strings.Contains(out.Hint, tt.wantHint) {
				t.Errorf("Hint = %q, want it to contain %q", out.Hint, tt.wantHint)
			}
		})
	}
}

func TestListMetricsEnrichment(t *testing.T) {
	ts := &toolset{prom: &fakeClient{
		// The Client contract says names arrive sorted; the fake honors it.
		names: []string{"orphan_metric", "up"},
		meta:  map[string]promclient.Meta{"up": {Type: "gauge", Help: "1 if the scrape succeeded"}},
	}}

	_, out, err := ts.listMetrics(context.Background(), nil, ListMetricsInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Metrics[1].Type != "gauge" || out.Metrics[1].Help == "" {
		t.Errorf(`"up" not enriched: %+v`, out.Metrics[1])
	}
	if out.Metrics[0].Type != "" || out.Metrics[0].Help != "" {
		t.Errorf(`"orphan_metric" should have empty type/help, got %+v`, out.Metrics[0])
	}
}

func TestListMetricsTruncation(t *testing.T) {
	many := make([]string, maxMetricNames+100)
	for i := range many {
		many[i] = fmt.Sprintf("metric_%04d", i)
	}
	ts := &toolset{prom: &fakeClient{names: many}}

	_, out, err := ts.listMetrics(context.Background(), nil, ListMetricsInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.Truncated {
		t.Error("Truncated = false, want true")
	}
	if len(out.Metrics) != maxMetricNames {
		t.Errorf("returned %d metrics, want cap %d", len(out.Metrics), maxMetricNames)
	}
	if out.Total != maxMetricNames+100 {
		t.Errorf("Total = %d, want %d", out.Total, maxMetricNames+100)
	}
	if !strings.Contains(out.Hint, "narrower filter") {
		t.Errorf("Hint = %q, want truncation guidance", out.Hint)
	}
}

func TestListMetricsUpstreamError(t *testing.T) {
	ts := &toolset{prom: &fakeClient{namesErr: fmt.Errorf("connection refused")}}

	_, _, err := ts.listMetrics(context.Background(), nil, ListMetricsInput{})
	if err == nil {
		t.Fatal("want error when Prometheus is unreachable, got nil")
	}
	// The SDK converts this into an isError tool result; the message is what
	// the agent reads, so it must name the fix.
	if !strings.Contains(err.Error(), "PROMSCOPE_PROMETHEUS_URL") {
		t.Errorf("error %q should tell the agent what to check", err)
	}
}
