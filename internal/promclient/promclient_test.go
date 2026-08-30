package promclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient starts a fake Prometheus that serves handler and returns a
// *HTTP pointed at it. httptest.Server listens on a real local port, so the
// full client_golang request path is exercised - URL building, JSON
// decoding, error mapping - without a real Prometheus.
func newTestClient(t *testing.T, handler http.HandlerFunc) *HTTP {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close) // no leaked listeners, however the test exits

	c, err := New(srv.URL, DefaultMaxResponseBytes)
	if err != nil {
		t.Fatalf("New(%q): %v", srv.URL, err)
	}
	return c
}

func TestMetricNames(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		want    []string
		wantErr bool
	}{
		{
			name:   "names returned sorted",
			status: http.StatusOK,
			// Prometheus /api/v1/label/__name__/values response shape.
			// Deliberately unsorted to prove we sort.
			body: `{"status":"success","data":["up","go_goroutines","vllm:num_requests_running"]}`,
			want: []string{"go_goroutines", "up", "vllm:num_requests_running"},
		},
		{
			name:   "empty prometheus",
			status: http.StatusOK,
			body:   `{"status":"success","data":[]}`,
			want:   []string{},
		},
		{
			name:    "upstream error surfaces",
			status:  http.StatusInternalServerError,
			body:    `{"status":"error","errorType":"internal","error":"boom"}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/label/__name__/values" {
					t.Errorf("unexpected path %q", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			})

			got, err := c.MetricNames(context.Background())
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestMetadata(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/metadata" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		// "up" has metadata, "orphan" has an empty entry list - the client
		// must skip it rather than invent an empty Meta for it.
		w.Write([]byte(`{"status":"success","data":{
			"up":[{"type":"gauge","help":"1 if the scrape succeeded","unit":""}],
			"orphan":[]
		}}`))
	})

	got, err := c.Metadata(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Meta{Type: "gauge", Help: "1 if the scrape succeeded"}
	if got["up"] != want {
		t.Errorf(`got["up"] = %+v, want %+v`, got["up"], want)
	}
	if _, ok := got["orphan"]; ok {
		t.Error(`"orphan" with empty metadata should be skipped`)
	}
}

func TestAlerts(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/alerts" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		// Pending listed before firing to prove we re-sort.
		w.Write([]byte(`{"status":"success","data":{"alerts":[
			{"labels":{"alertname":"ZDiskFilling","severity":"warn"},"annotations":{},"state":"pending","activeAt":"2026-08-27T22:30:00Z","value":"0.86"},
			{"labels":{"alertname":"HighTTFT","severity":"page"},"annotations":{"summary":"p99 TTFT above SLO"},"state":"firing","activeAt":"2026-08-27T22:00:00Z","value":"0.31"}
		]}}`))
	})

	got, err := c.Alerts(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d alerts, want 2", len(got))
	}
	if got[0].Name != "HighTTFT" || got[0].State != "firing" {
		t.Errorf("firing alert should sort first, got %+v", got[0])
	}
	if got[0].Annotations["summary"] != "p99 TTFT above SLO" {
		t.Errorf("annotations not converted: %+v", got[0].Annotations)
	}
	if got[1].Labels["severity"] != "warn" {
		t.Errorf("labels not converted: %+v", got[1].Labels)
	}
}

func TestQueryInstantVector(t *testing.T) {
	var gotTimeout string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		r.ParseForm()
		gotTimeout = r.Form.Get("timeout")
		w.Header().Set("Content-Type", "application/json")
		// Two series, deliberately out of label order to prove sorting.
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"__name__":"up","job":"z-last"},"value":[1724800000.123,"1"]},
			{"metric":{"__name__":"up","job":"a-first"},"value":[1724800000.123,"0"]}
		]}}`))
	})

	series, warns, err := c.Query(context.Background(), "up", time.Now(), 9*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	if gotTimeout != "9s" && gotTimeout != "9" {
		t.Errorf("timeout param = %q, want 9s - the upstream budget must be on the wire", gotTimeout)
	}
	if len(series) != 2 {
		t.Fatalf("got %d series, want 2", len(series))
	}
	if series[0].Labels["job"] != "a-first" {
		t.Errorf("series not sorted by labels: first is %v", series[0].Labels)
	}
	if len(series[0].Points) != 1 || series[0].Points[0].V != 0 {
		t.Errorf("instant query should yield one point per series, got %+v", series[0].Points)
	}
}

func TestQueryRangeMatrix(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		r.ParseForm()
		for _, p := range []string{"start", "end", "step"} {
			if r.Form.Get(p) == "" {
				t.Errorf("range param %q missing from request", p)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"__name__":"go_goroutines"},"values":[[1724800000,"33"],[1724800030,"35"],[1724800060,"34"]]}
		]}}`))
	})

	series, _, err := c.QueryRange(context.Background(),
		"go_goroutines", time.Now().Add(-time.Hour), time.Now(), 30*time.Second, 9*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(series) != 1 || len(series[0].Points) != 3 {
		t.Fatalf("want 1 series with 3 points, got %+v", series)
	}
	if series[0].Points[1].V != 35 {
		t.Errorf("point conversion wrong: %+v", series[0].Points)
	}
	if series[0].Points[0].T.After(series[0].Points[2].T) {
		t.Error("points out of chronological order")
	}
}

func TestQueryResponseCap(t *testing.T) {
	// A tiny configured cap against a response that exceeds it: the failure
	// must be the cap, reported with the configured size and the fix.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[`))
		w.Write([]byte(strings.Repeat(" ", 2048)))
		w.Write([]byte(`]}}`))
	}))
	t.Cleanup(srv.Close)

	c, err := New(srv.URL, 512)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, _, err = c.Query(context.Background(), "up", time.Now(), 9*time.Second)
	if err == nil {
		t.Fatal("want error when response exceeds cap, got nil")
	}
	if !strings.Contains(err.Error(), "512-byte cap") || !strings.Contains(err.Error(), "aggregate") {
		t.Errorf("error should name the configured cap and the fix, got: %v", err)
	}
}

func TestNewRejectsNonPositiveCap(t *testing.T) {
	if _, err := New("http://localhost:9090", 0); err == nil {
		t.Fatal("want error for zero byte cap, got nil")
	}
}

func TestContextCancellationPropagates(t *testing.T) {
	// The handler stalls longer than the caller's budget. If context
	// propagation works, the client gives up at ~50ms with an error;
	// if it doesn't, this test itself times out at the full second.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(1 * time.Second):
		case <-r.Context().Done(): // server side sees the abort too
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.MetricNames(ctx)
	if err == nil {
		t.Fatal("want error after context deadline, got nil")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("client ignored context: returned after %v", elapsed)
	}
}
