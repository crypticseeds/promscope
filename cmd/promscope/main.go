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
	"github.com/crypticseeds/promscope/internal/tools"
)

const version = "0.1.0"

// config holds all runtime configuration. It is read once at boot and
// immutable afterwards: mutable runtime config would need coordination
// across replicas, which is state (SPEC section 5).
type config struct {
	listenAddr    string
	prometheusURL string
	stateless     bool
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

func parseConfig() config {
	var cfg config
	flag.StringVar(&cfg.listenAddr, "listen",
		envOr("PROMSCOPE_LISTEN_ADDR", ":8090"),
		"HTTP listen address")
	flag.StringVar(&cfg.prometheusURL, "prometheus-url",
		envOr("PROMSCOPE_PROMETHEUS_URL", "http://localhost:9090"),
		"Prometheus base URL (consumed from M2 on)")
	flag.BoolVar(&cfg.stateless, "stateless",
		envBool("PROMSCOPE_STATELESS", true),
		"run the MCP transport stateless (false exists solely for the M5 A/B experiment)")
	flag.Parse()
	return cfg
}

func main() {
	cfg := parseConfig()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// The MCP server: promscope's identity in the initialize handshake.
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "promscope",
		Title:   "promscope - Prometheus over MCP",
		Version: version,
	}, nil)

	// The Prometheus client is constructed once and shared by every request:
	// it is stateless (an http.Client and a base URL), so replicas stay
	// interchangeable. Construction only fails on an unparseable URL.
	promClient, err := promclient.New(cfg.prometheusURL)
	if err != nil {
		logger.Error("invalid -prometheus-url", "url", cfg.prometheusURL, "error", err)
		os.Exit(2)
	}
	tools.Register(server, promClient)

	// The getServer callback exists so multi-tenant deployments can choose a
	// server per request; we always return the same one. Stateless mode gives
	// every POST a throwaway pre-initialized session, never issues or checks
	// Mcp-Session-Id, and rejects GET/DELETE with 405.
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless: cfg.stateless,
			Logger:    logger,
			// Client gone == answer undeliverable == stop working. This is
			// what will cancel in-flight Prometheus queries in M2 when the
			// agent disconnects mid-call.
			PropagateRequestCancellation: true,
		},
	)

	// Not "POST /mcp": in the stateful A/B configuration the handler also
	// serves GET (SSE stream) and DELETE (session teardown). Method policy
	// belongs to the SDK, which knows which mode it is in.
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)

	httpServer := &http.Server{
		Addr:    cfg.listenAddr,
		Handler: mux,
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
