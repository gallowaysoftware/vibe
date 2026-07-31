// recall is the fleet's personal-memory service (topology.md UC3 app):
// git-backed markdown memory per domain expert, exposed two ways —
// MCP tools at /mcp for in-conversation reads and writes, and the
// injection digest at /digest for harness hooks (Open WebUI inlet
// filter, Claude Code session hook) to prepend at chat start.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gallowaysoftware/vibe/internal/recall/mcpserver"
	"github.com/gallowaysoftware/vibe/internal/recall/store"
)

var version = "dev"

func main() {
	// Config is two env vars; a config package would be ceremony.
	root := envOr("RECALL_ROOT", "/data/memories")
	port := envOr("PORT", "8092")
	if os.Getenv("TARGET_ENV") != "local" {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	}

	st := store.New(root)
	if err := st.EnsureRepo(); err != nil {
		slog.Error("recall: memory root init failed", "root", root, "error", err)
		os.Exit(1)
	}

	deps := mcpserver.Deps{Store: st, Version: version}
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.Handler(deps))
	mux.Handle("GET /digest", mcpserver.DigestHandler(deps))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("recall: listening", "port", port, "root", root, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("recall: server failed", "error", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("recall: shutdown failed", "error", err)
	}
	slog.Info("recall: stopped")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// statusWriter records the status AND passes Flush through — the MCP
// Streamable HTTP transport uses SSE, which dies behind a non-Flusher
// wrapper (same lesson as subkb's server).
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		if r.URL.Path == "/healthz" {
			return
		}
		slog.Info("recall: request", "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "duration", time.Since(start).Round(time.Millisecond).String())
	})
}
