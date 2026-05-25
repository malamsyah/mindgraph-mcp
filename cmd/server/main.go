package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/mark3labs/mcp-go/server"

	"github.com/malamsyah/mindgraph-mcp/internal/auth"
	"github.com/malamsyah/mindgraph-mcp/internal/config"
	"github.com/malamsyah/mindgraph-mcp/internal/embeddings"
	mcpserver "github.com/malamsyah/mindgraph-mcp/internal/mcp"
	"github.com/malamsyah/mindgraph-mcp/internal/memory"
	"github.com/malamsyah/mindgraph-mcp/internal/reembed"
)

func main() {
	_ = godotenv.Load()

	cfg, err := config.LoadFromEnv()
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	repo, err := connectNeo4j(ctx, cfg)
	if err != nil {
		slog.Error("neo4j connect failed", "err", err, "uri", cfg.Neo4jURI, "user", cfg.Neo4jUser)
		os.Exit(1)
	}
	defer func() {
		_ = repo.Close(context.Background())
	}()

	if err := repo.Bootstrap(ctx, cfg.EmbeddingDimensions); err != nil {
		slog.Error("schema bootstrap failed", "err", err)
		os.Exit(1)
	}
	slog.Info("schema bootstrap complete")

	embedder := embeddings.NewVoyageClient(cfg.VoyageAPIKey, cfg.EmbeddingModel, cfg.EmbeddingDimensions, nil)
	go backfillMissingEmbeddings(ctx, repo, embedder)

	mcpSrv := mcpserver.NewServer(mcpserver.NewHandlers(repo, embedder, cfg.SuggestLinksThreshold))
	streamable := server.NewStreamableHTTPServer(mcpSrv)

	httpSrv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           withRequestID(auth.Middleware(cfg.APIKey)(streamable)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server error", "err", err)
			cancel()
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sigCh:
		slog.Info("shutdown signal received")
	case <-ctx.Done():
		slog.Info("context cancelled; shutting down")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown error", "err", err)
	}
}

// connectNeo4j connects to Neo4j with bounded retry, per-attempt timeout, and
// structured logging. Auth/security failures abort the retry loop early since
// retrying won't fix them.
func connectNeo4j(ctx context.Context, cfg *config.Config) (*memory.Repository, error) {
	const (
		maxAttempts    = 5
		attemptTimeout = 10 * time.Second
		baseBackoff    = 1 * time.Second
		maxBackoff     = 16 * time.Second
	)
	slog.Info("connecting to neo4j", "uri", cfg.Neo4jURI, "user", cfg.Neo4jUser, "max_attempts", maxAttempts)

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		repo, err := memory.NewRepository(attemptCtx, cfg.Neo4jURI, cfg.Neo4jUser, cfg.Neo4jPassword)
		cancel()
		if err == nil {
			slog.Info("neo4j connect succeeded", "attempt", attempt)
			return repo, nil
		}
		lastErr = err
		if memory.IsPermanentConnectError(err) {
			return nil, fmt.Errorf("permanent error on attempt %d: %w", attempt, err)
		}
		if attempt == maxAttempts {
			break
		}
		backoff := connectBackoff(attempt, baseBackoff, maxBackoff)
		slog.Warn("neo4j connect attempt failed; will retry",
			"err", err, "attempt", attempt, "next_attempt_in", backoff.String())
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, fmt.Errorf("connect cancelled during backoff after attempt %d: %w", attempt, ctx.Err())
		}
	}
	return nil, fmt.Errorf("exhausted %d attempts: %w", maxAttempts, lastErr)
}

// connectBackoff returns base * 2^(attempt-1), capped at max.
func connectBackoff(attempt int, base, max time.Duration) time.Duration {
	d := base << (attempt - 1)
	if d <= 0 || d > max {
		return max
	}
	return d
}

// backfillMissingEmbeddings runs once at startup, embedding any memories that
// were persisted without one (e.g. created during Voyage outages or Phase 1).
// Shares its implementation with the reembed_memories MCP tool so a manual
// re-run behaves identically to the boot pass.
func backfillMissingEmbeddings(ctx context.Context, repo *memory.Repository, embedder embeddings.Embedder) {
	if embedder == nil {
		return
	}
	result, err := reembed.Run(ctx, repo, embedder, reembed.Options{Scope: reembed.ScopeMissing})
	if err != nil {
		slog.Error("backfill failed", "err", err, "processed", processed(result), "succeeded", succeeded(result))
		return
	}
	if result.Processed > 0 {
		slog.Info("backfill complete", "processed", result.Processed, "succeeded", result.Succeeded, "failed", result.Failed)
	}
}

func processed(r *reembed.Result) int {
	if r == nil {
		return 0
	}
	return r.Processed
}

func succeeded(r *reembed.Result) int {
	if r == nil {
		return 0
	}
	return r.Succeeded
}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newRequestID()
		}
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r)
	})
}

type requestIDKey struct{}

func newRequestID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
