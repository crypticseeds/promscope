package tools

import (
	"strings"
	"testing"
	"time"
)

func TestLimitsValidate(t *testing.T) {
	mod := func(f func(*Limits)) Limits {
		l := DefaultLimits()
		f(&l)
		return l
	}

	tests := []struct {
		name    string
		limits  Limits
		wantErr string // substring; "" means valid
	}{
		{"defaults are valid", DefaultLimits(), ""},
		{"zero series", mod(func(l *Limits) { l.MaxSeries = 0 }), "positive"},
		{"negative lookback", mod(func(l *Limits) { l.MaxLookback = -time.Hour }), "positive"},
		{"one point per series", mod(func(l *Limits) { l.MaxPointsPerSeries = 1 }), "at least 2"},
		{"sub-second timeout", mod(func(l *Limits) { l.QueryTimeout = 500 * time.Millisecond }), "1s floor"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.limits.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %v does not contain %q", err, tt.wantErr)
			}
		})
	}
}
