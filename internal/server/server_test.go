package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crypticseeds/promscope/internal/promclient"
)

// errClient fails every operation - the decorator must count that.
type errClient struct{ err error }

func (e *errClient) MetricNames(context.Context) ([]string, error) { return nil, e.err }
func (e *errClient) Metadata(context.Context) (map[string]promclient.Meta, error) {
	return nil, e.err
}
func (e *errClient) Alerts(context.Context) ([]promclient.Alert, error)   { return nil, e.err }
func (e *errClient) Rules(context.Context) ([]promclient.RuleGroup, error) { return nil, e.err }
func (e *errClient) Query(context.Context, string, time.Time, time.Duration) ([]promclient.Series, []string, error) {
	return nil, nil, e.err
}
func (e *errClient) QueryRange(context.Context, string, time.Time, time.Time, time.Duration, time.Duration) ([]promclient.Series, []string, error) {
	return nil, nil, e.err
}

func TestHealthz(t *testing.T) {
	s := New("test-version")
	srv := httptest.NewServer(s.Handler(http.NotFoundHandler()))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "test-version") {
		t.Errorf("healthz should report the version, got %s", body)
	}
}

func TestMCPRequestsAreCounted(t *testing.T) {
	s := New("test")
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(s.Handler(inner))
	t.Cleanup(srv.Close)

	for range 3 {
		if _, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader("{}")); err != nil {
			t.Fatalf("POST /mcp: %v", err)
		}
	}

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), `promscope_http_requests_total{code="200"} 3`) {
		t.Errorf("/metrics should count the 3 MCP requests, got:\n%s",
			firstMatching(string(body), "promscope_http_requests_total"))
	}
	if !strings.Contains(string(body), "promscope_http_request_duration_seconds") {
		t.Error("/metrics should expose the duration histogram")
	}
	if !strings.Contains(string(body), "go_goroutines") {
		t.Error("/metrics should include the standard Go collector")
	}
}

func TestWrapClientCountsOutcomes(t *testing.T) {
	s := New("test")
	wrapped := s.WrapClient(&errClient{err: fmt.Errorf("boom")})

	_, _ = wrapped.MetricNames(context.Background())
	_, _ = wrapped.MetricNames(context.Background())
	_, _, _ = wrapped.Query(context.Background(), "up", time.Now(), time.Second)

	if got := testutil.ToFloat64(s.upstream.WithLabelValues("metric_names", "error")); got != 2 {
		t.Errorf("metric_names error count = %v, want 2", got)
	}
	if got := testutil.ToFloat64(s.upstream.WithLabelValues("query", "error")); got != 1 {
		t.Errorf("query error count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(s.upstream.WithLabelValues("metric_names", "ok")); got != 0 {
		t.Errorf("ok count = %v, want 0 - everything failed", got)
	}
}

// firstMatching keeps failure output readable: only the relevant lines.
func firstMatching(body, substr string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
