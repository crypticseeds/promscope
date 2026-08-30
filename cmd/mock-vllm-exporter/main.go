// Command mock-vllm-exporter serves vLLM-shaped Prometheus metrics without a
// GPU. It exists so the demo stack, screenshots, and load tests speak the
// language of LLM inference (TTFT, tokens/sec, KV-cache usage) instead of
// disk io - and so promscope can be developed against realistic series with
// zero RunPod spend.
//
// Metric names are copied verbatim from vLLM's exposition (colons and all -
// legal in the Prometheus data model). Only the values are simulated: gauges
// random-walk inside plausible bounds and histograms draw latencies shaped
// like a busy inference server. Swapping this container for a real vLLM
// endpoint is a one-line prometheus.yml change (SPEC section 7, D1).
//
// Note: `promtool check metrics` flags the colons - by convention they are
// reserved for recording rules. Real vLLM violates that convention too, and
// fidelity to real vLLM is this binary's entire purpose, so the lint is
// accepted, not fixed.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	listen := flag.String("listen", ":9105", "HTTP listen address")
	model := flag.String("model", "llama-3.1-8b-instruct", "model_name label value")
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	reg := prometheus.NewRegistry()
	labels := prometheus.Labels{"model_name": *model}

	running := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vllm:num_requests_running", Help: "Number of requests currently running on GPU.", ConstLabels: labels})
	waiting := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vllm:num_requests_waiting", Help: "Number of requests waiting to be processed.", ConstLabels: labels})
	cacheUsage := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vllm:gpu_cache_usage_perc", Help: "GPU KV-cache usage. 1 means 100 percent usage.", ConstLabels: labels})
	ttft := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "vllm:time_to_first_token_seconds", Help: "Histogram of time to first token in seconds.",
		ConstLabels: labels,
		Buckets:     []float64{0.01, 0.025, 0.05, 0.075, 0.1, 0.15, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10}})
	tpot := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "vllm:time_per_output_token_seconds", Help: "Histogram of time per output token in seconds.",
		ConstLabels: labels,
		Buckets:     []float64{0.01, 0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.3, 0.5, 0.75, 1}})
	e2e := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "vllm:e2e_request_latency_seconds", Help: "Histogram of end to end request latency in seconds.",
		ConstLabels: labels,
		Buckets:     []float64{0.3, 0.5, 0.8, 1, 1.5, 2, 2.5, 5, 10, 15, 20, 30}})
	promptTokens := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vllm:prompt_tokens_total", Help: "Number of prefill tokens processed.", ConstLabels: labels})
	genTokens := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "vllm:generation_tokens_total", Help: "Number of generation tokens processed.", ConstLabels: labels})
	reg.MustRegister(running, waiting, cacheUsage, ttft, tpot, e2e, promptTokens, genTokens)

	// The simulation: every 2s a "batch" of requests completes. Gauges
	// random-walk within plausible bounds; latency samples are drawn around
	// a busy-server profile; token counters advance accordingly.
	go func() {
		load, cache := 12.0, 0.55 // starting points of the random walks
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for range tick.C {
			load = walk(load, 4, 0, 64)
			cache = walk(cache, 0.05, 0.15, 0.97)
			running.Set(float64(int(load)))
			waiting.Set(float64(int(walk(load/3, 2, 0, 32))))
			cacheUsage.Set(cache)

			completed := 1 + rand.IntN(6)
			for range completed {
				ttft.Observe(0.08 + rand.Float64()*0.4*(0.5+cache)) // busier cache, slower TTFT
				tpot.Observe(0.02 + rand.Float64()*0.05)
				e2e.Observe(0.5 + rand.Float64()*4)
				promptTokens.Add(float64(64 + rand.IntN(1024)))
				genTokens.Add(float64(32 + rand.IntN(512)))
			}
		}
	}()

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		logger.Info("mock-vllm-exporter listening", "addr", *listen, "model", *model)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// walk advances a bounded random walk: one step of at most maxStep in either
// direction, clamped to [lo, hi].
func walk(v, maxStep, lo, hi float64) float64 {
	v += (rand.Float64()*2 - 1) * maxStep
	return min(max(v, lo), hi)
}
