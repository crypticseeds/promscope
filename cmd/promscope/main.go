// Command promscope is a stateless MCP server exposing read-only Prometheus
// access for AI agents. M1: transport skeleton only - no tools, no resources.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crypticseeds/promscope/internal/promclient"
	"github.com/crypticseeds/promscope/internal/server"
	"github.com/crypticseeds/promscope/internal/tools"
)

const version = "0.1.0"

// config holds all runtime configuration. It is read once at boot and
// immutable afterwards: mutable runtime config would need coordination
// across replicas, which is state (SPEC section 5).
type config struct {
	listenAddr       string
	prometheusURL    string
	stateless        bool
	limits           tools.Limits
	maxResponseBytes int
}

// envOr returns the environment variable's value, or def if unset/empty.
// Precedence: flag > env > default.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool is envOr for booleans. A malformed value is a hard error, not a
// silent default: the stateless toggle drives the M5 A/B experiment, and a
// typo silently flipping modes would invalidate its results.
func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promscope: %s=%q is not a boolean\n", key, v)
		os.Exit(2)
	}
	return b
}

// envDuration and envInt follow the same fail-hard contract as envBool:
// guardrails silently falling back to defaults would be worse than a crash.
func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promscope: %s=%q is not a duration (e.g. 30s, 24h)\n", key, v)
		os.Exit(2)
	}
	return d
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "promscope: %s=%q is not an integer\n", key, v)
		os.Exit(2)
	}
	return n
}

func parseConfig() config {
	var cfg config
	def := tools.DefaultLimits()
	flag.StringVar(&cfg.listenAddr, "listen",
		envOr("PROMSCOPE_LISTEN_ADDR", ":8090"),
		"HTTP listen address")
	flag.StringVar(&cfg.prometheusURL, "prometheus-url",
		envOr("PROMSCOPE_PROMETHEUS_URL", "http://localhost:9090"),
		"Prometheus base URL")
	flag.BoolVar(&cfg.stateless, "stateless",
		envBool("PROMSCOPE_STATELESS", true),
		"run the MCP transport stateless (false exists solely for the M5 A/B experiment)")
	flag.DurationVar(&cfg.limits.MaxLookback, "max-lookback",
		envDuration("PROMSCOPE_MAX_LOOKBACK", def.MaxLookback),
		"widest allowed range-query window")
	flag.IntVar(&cfg.limits.MaxSeries, "max-series",
		envInt("PROMSCOPE_MAX_SERIES", def.MaxSeries),
		"series returned per query before truncation")
	flag.IntVar(&cfg.limits.MaxPointsPerSeries, "max-points",
		envInt("PROMSCOPE_MAX_POINTS", def.MaxPointsPerSeries),
		"points per series budget for range queries")
	flag.DurationVar(&cfg.limits.QueryTimeout, "query-timeout",
		envDuration("PROMSCOPE_QUERY_TIMEOUT", def.QueryTimeout),
		"outer per-query budget; Prometheus gets 90% of it")
	flag.IntVar(&cfg.limits.MaxMetricNames, "max-metric-names",
		envInt("PROMSCOPE_MAX_METRIC_NAMES", def.MaxMetricNames),
		"list_metrics cap before truncation")
	flag.IntVar(&cfg.maxResponseBytes, "max-response-bytes",
		envInt("PROMSCOPE_MAX_RESPONSE_BYTES", promclient.DefaultMaxResponseBytes),
		"cap on any Prometheus response body in bytes")
	flag.Parse()
	return cfg
}

func main() {
	cfg := parseConfig()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := cfg.limits.Validate(); err != nil {
		logger.Error("invalid limits", "error", err)
		os.Exit(2)
	}

	// The MCP server: promscope's identity in the initialize handshake.
	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "promscope",
		Title:   "promscope - Prometheus over MCP",
		Version: version,
	}, nil)

	// The Prometheus client is constructed once and shared by every request:
	// it is stateless (an http.Client and a base URL), so replicas stay
	// interchangeable. Construction only fails on an unparseable URL.
	// Self-observability first: the client is wrapped so every upstream call
	// is counted, then handed to the tools. Decorating the interface means
	// neither promclient nor the handlers know instrumentation exists.
	obs := server.New(version)

	promClient, err := promclient.New(cfg.prometheusURL, int64(cfg.maxResponseBytes))
	if err != nil {
		logger.Error("invalid prometheus client config", "url", cfg.prometheusURL, "error", err)
		os.Exit(2)
	}
	tools.Register(mcpServer, obs.WrapClient(promClient), cfg.limits)

	// The getServer callback exists so multi-tenant deployments can choose a
	// server per request; we always return the same one. Stateless mode gives
	// every POST a throwaway pre-initialized session, never issues or checks
	// Mcp-Session-Id, and rejects GET/DELETE with 405.
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{
			Stateless: cfg.stateless,
			Logger:    logger,
			// Client gone == answer undeliverable == stop working. This is
			// what will cancel in-flight Prometheus queries in M2 when the
			// agent disconnects mid-call.
			PropagateRequestCancellation: true,
		},
	)

	// The full HTTP surface: instrumented /mcp, /healthz, /metrics. In the
	// stateful A/B configuration the MCP handler also serves GET (SSE) and
	// DELETE (session teardown); method policy belongs to the SDK.
	httpServer := &http.Server{
		Addr:    cfg.listenAddr,
		Handler: obs.Handler(mcpHandler),
		// Slowloris protection. No WriteTimeout: it would sever long-lived
		// SSE responses in the stateful configuration.
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Graceful shutdown: SIGINT/SIGTERM stops accepting connections and lets
	// in-flight requests drain, bounded by a deadline.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("promscope listening",
			"addr", cfg.listenAddr,
			"stateless", cfg.stateless,
			"prometheus_url", cfg.prometheusURL,
			"limits", fmt.Sprintf("%+v", cfg.limits),
			"max_response_bytes", cfg.maxResponseBytes,
			"version", version)
		if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		logger.Error("server failed", "error", err)
		os.Exit(1)
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown incomplete", "error", err)
		os.Exit(1)
	}
}
