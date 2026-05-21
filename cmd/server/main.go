package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

	repo, err := memory.NewRepository(ctx, cfg.Neo4jURI, cfg.Neo4jUser, cfg.Neo4jPassword)
	if err != nil {
		slog.Error("neo4j connect failed", "err", err)
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

	mcpSrv := mcpserver.NewServer(mcpserver.NewHandlers(repo, embedder))
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

// backfillMissingEmbeddings runs once at startup, embedding any memories that
// were persisted without one (e.g. created during Voyage outages or Phase 1).
// Batches of 128 keep request payloads sane.
func backfillMissingEmbeddings(ctx context.Context, repo *memory.Repository, embedder embeddings.Embedder) {
	if embedder == nil {
		return
	}
	const batch = 128
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		pending, err := repo.MissingEmbeddings(ctx, batch)
		if err != nil {
			slog.Error("backfill: query missing embeddings", "err", err)
			return
		}
		if len(pending) == 0 {
			if total > 0 {
				slog.Info("backfill complete", "embedded", total)
			}
			return
		}
		texts := make([]string, len(pending))
		for i, m := range pending {
			texts[i] = m.Content
		}
		vecs, err := embedder.Embed(ctx, texts, embeddings.InputDocument)
		if err != nil {
			slog.Error("backfill embed failed; aborting backfill", "err", err)
			return
		}
		for i, m := range pending {
			if i >= len(vecs) {
				break
			}
			if err := repo.UpdateEmbedding(ctx, m.ID, vecs[i]); err != nil {
				slog.Error("backfill update embedding", "id", m.ID, "err", err)
			}
		}
		total += len(pending)
		if len(pending) < batch {
			slog.Info("backfill complete", "embedded", total)
			return
		}
	}
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
