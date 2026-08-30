// Package promclient is promscope's narrow view of the Prometheus HTTP API.
//
// Tool handlers depend on the Client interface, never on client_golang types
// directly: the interface is small enough to fake in tests, and it stops
// upstream types leaking through the rest of the codebase (decision D2).
package promclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
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

// Point is one sample: a timestamp and a value. V may be NaN or ±Inf -
// PromQL produces those (e.g. 0/0) - and callers that serialize to JSON
// must handle them, because encoding/json cannot.
type Point struct {
	T time.Time
	V float64
}

// Series is one time series in a query result: a label set and its samples.
// An instant query yields exactly one point per series.
type Series struct {
	Labels map[string]string
	Points []Point
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

	// Query evaluates a PromQL expression at time ts. The timeout is
	// passed to Prometheus as its own evaluation deadline; keep it below
	// the ctx deadline so the upstream gives up first and returns a clean
	// error. Warnings are advisory notes from Prometheus, not failures.
	Query(ctx context.Context, promql string, ts time.Time, timeout time.Duration) (result []Series, warnings []string, err error)

	// QueryRange evaluates a PromQL expression over [start, end] at the
	// given step. Same timeout semantics as Query.
	QueryRange(ctx context.Context, promql string, start, end time.Time, step time.Duration, timeout time.Duration) (result []Series, warnings []string, err error)
}

// HTTP is the real Client, backed by the official client_golang v1 API.
type HTTP struct {
	api      promv1.API
	maxBytes int64
}

// Compile-time proof that *HTTP satisfies Client: if the method set drifts,
// the build breaks here instead of at a distant call site.
var _ Client = (*HTTP)(nil)

// DefaultMaxResponseBytes caps any Prometheus response body (SPEC section
// 4). A pathological query can return tens of MB; the cap turns that into a
// clean error instead of an OOM or a context-window bomb downstream.
const DefaultMaxResponseBytes = 1 << 20 // 1 MiB

// errBodyTooLarge is detected by substring (see tooLarge) because
// client_golang does not always preserve wrapped errors across its reads.
var errBodyTooLarge = errors.New("promclient: response exceeded body cap")

// limitRoundTripper enforces the byte cap on every response body by
// wrapping it in a reader that fails once the cap is crossed.
type limitRoundTripper struct {
	next http.RoundTripper
	max  int64
}

func (l limitRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := l.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &cappedBody{rc: resp.Body, remaining: l.max}
	return resp, nil
}

type cappedBody struct {
	rc        io.ReadCloser
	remaining int64
}

func (c *cappedBody) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		return 0, errBodyTooLarge
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.rc.Read(p)
	c.remaining -= int64(n)
	return n, err
}

func (c *cappedBody) Close() error { return c.rc.Close() }

// tooLarge reports whether err is (or wraps, however lossily) the body cap.
func tooLarge(err error) bool {
	return err != nil &&
		(errors.Is(err, errBodyTooLarge) || strings.Contains(err.Error(), errBodyTooLarge.Error()))
}

// New returns an HTTP client for the Prometheus server at baseURL.
// maxResponseBytes caps every response body; use DefaultMaxResponseBytes
// unless configured otherwise.
func New(baseURL string, maxResponseBytes int64) (*HTTP, error) {
	if maxResponseBytes <= 0 {
		return nil, fmt.Errorf("promclient: response byte cap must be positive, got %d", maxResponseBytes)
	}
	c, err := api.NewClient(api.Config{
		Address:      baseURL,
		RoundTripper: limitRoundTripper{next: api.DefaultRoundTripper, max: maxResponseBytes},
	})
	if err != nil {
		return nil, fmt.Errorf("promclient: %w", err)
	}
	return &HTTP{api: promv1.NewAPI(c), maxBytes: maxResponseBytes}, nil
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

func (h *HTTP) Query(ctx context.Context, promql string, ts time.Time, timeout time.Duration) ([]Series, []string, error) {
	val, warns, err := h.api.Query(ctx, promql, ts, promv1.WithTimeout(timeout))
	if err != nil {
		return nil, nil, h.queryErr("query", err)
	}
	series, err := toSeries(val)
	if err != nil {
		return nil, nil, err
	}
	return series, warns, nil
}

func (h *HTTP) QueryRange(ctx context.Context, promql string, start, end time.Time, step time.Duration, timeout time.Duration) ([]Series, []string, error) {
	r := promv1.Range{Start: start, End: end, Step: step}
	val, warns, err := h.api.QueryRange(ctx, promql, r, promv1.WithTimeout(timeout))
	if err != nil {
		return nil, nil, h.queryErr("range query", err)
	}
	series, err := toSeries(val)
	if err != nil {
		return nil, nil, err
	}
	return series, warns, nil
}

// queryErr maps upstream failures to actionable messages, special-casing the
// response cap: the fix for "too large" is a narrower query, and only this
// layer knows that is what happened.
func (h *HTTP) queryErr(op string, err error) error {
	if tooLarge(err) {
		return fmt.Errorf("promclient: %s: response exceeded the %d-byte cap - aggregate (e.g. avg by(...)) or narrow the selector", op, h.maxBytes)
	}
	return fmt.Errorf("promclient: %s: %w", op, err)
}

// toSeries converts the three PromQL result shapes into one: a list of
// labeled series. Vector = one point per series (instant), Matrix = many
// points (range), Scalar = one unlabeled series. Sorted by label string so
// output is deterministic across calls - tests stay simple and agent prompt
// caches stay warm.
func toSeries(v model.Value) ([]Series, error) {
	var out []Series
	switch val := v.(type) {
	case model.Vector:
		out = make([]Series, 0, len(val))
		for _, s := range val {
			out = append(out, Series{
				Labels: labelSetToMap(model.LabelSet(s.Metric)),
				Points: []Point{{T: s.Timestamp.Time(), V: float64(s.Value)}},
			})
		}
	case model.Matrix:
		out = make([]Series, 0, len(val))
		for _, ss := range val {
			pts := make([]Point, len(ss.Values))
			for i, p := range ss.Values {
				pts[i] = Point{T: p.Timestamp.Time(), V: float64(p.Value)}
			}
			out = append(out, Series{
				Labels: labelSetToMap(model.LabelSet(ss.Metric)),
				Points: pts,
			})
		}
	case *model.Scalar:
		out = []Series{{
			Labels: map[string]string{},
			Points: []Point{{T: val.Timestamp.Time(), V: float64(val.Value)}},
		}}
	default:
		return nil, fmt.Errorf("promclient: unsupported result type %q", v.Type())
	}
	sort.Slice(out, func(i, j int) bool {
		return labelsKey(out[i].Labels) < labelsKey(out[j].Labels)
	})
	return out, nil
}

// labelsKey is a cheap deterministic ordering key for a label set.
func labelsKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte(',')
	}
	return b.String()
}
