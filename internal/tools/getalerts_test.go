package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crypticseeds/promscope/internal/promclient"
)

var testAlerts = []promclient.Alert{
	{
		Name:        "HighTTFT",
		State:       "firing",
		Labels:      map[string]string{"alertname": "HighTTFT", "severity": "page"},
		Annotations: map[string]string{"summary": "p99 TTFT above SLO"},
		ActiveAt:    time.Date(2026, 8, 27, 22, 0, 0, 0, time.UTC),
		Value:       "0.31",
	},
	{
		Name:     "DiskFilling",
		State:    "pending",
		Labels:   map[string]string{"alertname": "DiskFilling"},
		ActiveAt: time.Date(2026, 8, 27, 22, 30, 0, 0, time.UTC),
		Value:    "0.86",
	},
}

func TestGetAlerts(t *testing.T) {
	tests := []struct {
		name      string
		client    *fakeClient
		in        GetAlertsInput
		wantNames []string
		wantErr   string // substring of the expected error; "" means no error
		wantHint  string
	}{
		{
			name:      "no filter returns everything",
			client:    &fakeClient{alerts: testAlerts},
			wantNames: []string{"HighTTFT", "DiskFilling"},
		},
		{
			name:      "state filter narrows to firing",
			client:    &fakeClient{alerts: testAlerts},
			in:        GetAlertsInput{State: "firing"},
			wantNames: []string{"HighTTFT"},
		},
		{
			name:    "invalid state names the valid values",
			client:  &fakeClient{alerts: testAlerts},
			in:      GetAlertsInput{State: "resolved"},
			wantErr: `"firing", "pending"`,
		},
		{
			name:      "zero alerts is explained, not silent",
			client:    &fakeClient{},
			wantNames: []string{},
			wantHint:  "no alert rules are configured",
		},
		{
			name:    "upstream failure tells the agent what to check",
			client:  &fakeClient{alertsErr: fmt.Errorf("connection refused")},
			wantErr: "PROMSCOPE_PROMETHEUS_URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := &toolset{prom: tt.client}
			_, out, err := ts.getAlerts(context.Background(), nil, tt.in)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out.Total != len(tt.wantNames) {
				t.Errorf("Total = %d, want %d", out.Total, len(tt.wantNames))
			}
			for i, want := range tt.wantNames {
				if out.Alerts[i].Name != want {
					t.Errorf("Alerts[%d].Name = %q, want %q", i, out.Alerts[i].Name, want)
				}
			}
			if tt.wantHint != "" && !strings.Contains(out.Hint, tt.wantHint) {
				t.Errorf("Hint = %q, want it to contain %q", out.Hint, tt.wantHint)
			}
			if out.Total > 0 && out.Alerts[0].ActiveAt == "" {
				t.Error("ActiveAt should be formatted, got empty")
			}
		})
	}
}
