// Package promclient is promscope's narrow view of the Prometheus HTTP API.
//
// Tool handlers depend on the Client interface, never on client_golang types
// directly: the interface is small enough to fake in tests, and it stops
// upstream types leaking through the rest of the codebase (decision D2).
package promclient

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// Meta is the metadata Prometheus holds for one metric name.
type Meta struct {
	Type string // counter | gauge | histogram | summary | unknown
	Help string
}

// Alert is one active alert. Firing means the condition has held for the
// rule's full "for" duration; pending means it is true but still waiting it
// out. Inactive alerts are not returned by Prometheus at all.
type Alert struct {
	Name        string            // the alertname label
	State       string            // firing | pending
	Labels      map[string]string // includes alertname
	Annotations map[string]string
	ActiveAt    time.Time
	Value       string // the alert expression's value when last evaluated
}

// Client is the slice of Prometheus that promscope's tools consume.
// It grows only when a tool needs a new operation (SPEC section 6).
type Client interface {
	// MetricNames returns every metric name Prometheus knows, sorted.
	MetricNames(ctx context.Context) ([]string, error)

	// Metadata returns type + help text per metric name. Prometheus only
	// holds metadata for actively-scraped series, so callers must tolerate
	// names that have no entry here.
	Metadata(ctx context.Context) (map[string]Meta, error)

	// Alerts returns all active (firing or pending) alerts, sorted firing
	// first, then by name.
	Alerts(ctx context.Context) ([]Alert, error)
}

// HTTP is the real Client, backed by the official client_golang v1 API.
type HTTP struct {
	api promv1.API
}

// Compile-time proof that *HTTP satisfies Client: if the method set drifts,
// the build breaks here instead of at a distant call site.
var _ Client = (*HTTP)(nil)

// New returns an HTTP client for the Prometheus server at baseURL.
func New(baseURL string) (*HTTP, error) {
	c, err := api.NewClient(api.Config{Address: baseURL})
	if err != nil {
		return nil, fmt.Errorf("promclient: %w", err)
	}
	return &HTTP{api: promv1.NewAPI(c)}, nil
}

func (h *HTTP) MetricNames(ctx context.Context) ([]string, error) {
	// __name__ is the reserved label whose values are the metric names
	// themselves. Zero time bounds mean "no time restriction". Warnings are
	// deliberately dropped: a warning-bearing name list is still useful.
	vals, _, err := h.api.LabelValues(ctx, "__name__", nil, time.Time{}, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("promclient: metric names: %w", err)
	}
	names := make([]string, len(vals))
	for i, v := range vals {
		names[i] = string(v)
	}
	sort.Strings(names)
	return names, nil
}

func (h *HTTP) Metadata(ctx context.Context) (map[string]Meta, error) {
	// Empty metric + limit means "all metadata". Prometheus returns a slice
	// per name because different scrape targets can disagree about a
	// metric's metadata; we take the first entry - good enough for hints.
	md, err := h.api.Metadata(ctx, "", "")
	if err != nil {
		return nil, fmt.Errorf("promclient: metadata: %w", err)
	}
	out := make(map[string]Meta, len(md))
	for name, entries := range md {
		if len(entries) == 0 {
			continue
		}
		out[name] = Meta{Type: string(entries[0].Type), Help: entries[0].Help}
	}
	return out, nil
}

func (h *HTTP) Alerts(ctx context.Context) ([]Alert, error) {
	res, err := h.api.Alerts(ctx)
	if err != nil {
		return nil, fmt.Errorf("promclient: alerts: %w", err)
	}
	out := make([]Alert, len(res.Alerts))
	for i, a := range res.Alerts {
		out[i] = Alert{
			Name:        string(a.Labels["alertname"]),
			State:       string(a.State),
			Labels:      labelSetToMap(a.Labels),
			Annotations: labelSetToMap(a.Annotations),
			ActiveAt:    a.ActiveAt,
			Value:       a.Value,
		}
	}
	// Deterministic order: firing before pending, then by name. Stable
	// output keeps tests simple and agent prompt caches warm.
	sort.Slice(out, func(i, j int) bool {
		if out[i].State != out[j].State {
			return out[i].State == "firing"
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func labelSetToMap(ls model.LabelSet) map[string]string {
	m := make(map[string]string, len(ls))
	for k, v := range ls {
		m[string(k)] = string(v)
	}
	return m
}
