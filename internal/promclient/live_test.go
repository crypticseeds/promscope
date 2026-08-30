package promclient

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveAgainstRealPrometheus is the manual-verification hook: it runs
// only when PROMSCOPE_LIVE_URL is set, so `go test ./...` stays hermetic.
//
//	docker run -d --rm --name prom -p 9090:9090 prom/prometheus
//	PROMSCOPE_LIVE_URL=http://localhost:9090 go test ./internal/promclient/ -run Live -v
//
// In GoLand: edit the test's run configuration and add the env var.
func TestLiveAgainstRealPrometheus(t *testing.T) {
	url := os.Getenv("PROMSCOPE_LIVE_URL")
	if url == "" {
		t.Skip("set PROMSCOPE_LIVE_URL to run the live smoke test")
	}

	c, err := New(url)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("instant query", func(t *testing.T) {
		series, warns, err := c.Query(ctx, "up", time.Now(), 9*time.Second)
		if err != nil {
			t.Fatalf("Query(up): %v", err)
		}
		if len(series) == 0 {
			t.Fatal("Query(up) returned no series - has Prometheus completed its first self-scrape (~15s)?")
		}
		for _, s := range series {
			t.Logf("up{job=%q instance=%q} = %v (warnings: %v)",
				s.Labels["job"], s.Labels["instance"], s.Points[0].V, warns)
		}
	})

	t.Run("range query", func(t *testing.T) {
		series, _, err := c.QueryRange(ctx, "go_goroutines",
			time.Now().Add(-2*time.Minute), time.Now(), 15*time.Second, 9*time.Second)
		if err != nil {
			t.Fatalf("QueryRange(go_goroutines): %v", err)
		}
		if len(series) == 0 {
			t.Fatal("QueryRange returned no series")
		}
		s := series[0]
		t.Logf("go_goroutines: %d points over 2m", len(s.Points))
		for _, p := range s.Points {
			t.Logf("  %s  %v", p.T.Format(time.RFC3339), p.V)
		}
	})
}
