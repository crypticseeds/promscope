package promclient

import (
	"context"
	"net/http"
	"net/http/httptest"
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

	c, err := New(srv.URL)
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
