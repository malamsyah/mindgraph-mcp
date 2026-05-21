package config

import (
	"errors"
	"testing"
)

func TestLoadFromEnv_AllRequiredSet(t *testing.T) {
	t.Setenv("MINDGRAPH_API_KEY", "test-key")
	t.Setenv("NEO4J_URI", "bolt://localhost:7687")
	t.Setenv("NEO4J_USER", "neo4j")
	t.Setenv("NEO4J_PASSWORD", "pw")
	t.Setenv("VOYAGE_API_KEY", "voyage-key")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}
	if cfg.APIKey != "test-key" {
		t.Errorf("APIKey = %q", cfg.APIKey)
	}
	if cfg.EmbeddingModel != "voyage-3-large" {
		t.Errorf("default EmbeddingModel = %q", cfg.EmbeddingModel)
	}
	if cfg.EmbeddingDimensions != 2048 {
		t.Errorf("default EmbeddingDimensions = %d", cfg.EmbeddingDimensions)
	}
	if cfg.Port != "8080" {
		t.Errorf("default Port = %q", cfg.Port)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("default LogLevel = %q", cfg.LogLevel)
	}
}

func TestLoadFromEnv_OverridesDefaults(t *testing.T) {
	t.Setenv("MINDGRAPH_API_KEY", "k")
	t.Setenv("NEO4J_URI", "x")
	t.Setenv("NEO4J_USER", "u")
	t.Setenv("NEO4J_PASSWORD", "p")
	t.Setenv("VOYAGE_API_KEY", "v")
	t.Setenv("EMBEDDING_MODEL", "voyage-3-lite")
	t.Setenv("EMBEDDING_DIMENSIONS", "1024")
	t.Setenv("PORT", "9090")
	t.Setenv("LOG_LEVEL", "debug")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv: %v", err)
	}
	if cfg.EmbeddingModel != "voyage-3-lite" {
		t.Errorf("EmbeddingModel = %q", cfg.EmbeddingModel)
	}
	if cfg.EmbeddingDimensions != 1024 {
		t.Errorf("EmbeddingDimensions = %d", cfg.EmbeddingDimensions)
	}
	if cfg.Port != "9090" {
		t.Errorf("Port = %q", cfg.Port)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q", cfg.LogLevel)
	}
}

func TestLoadFromEnv_MissingRequired(t *testing.T) {
	t.Setenv("MINDGRAPH_API_KEY", "")
	t.Setenv("NEO4J_URI", "x")
	t.Setenv("NEO4J_USER", "u")
	t.Setenv("NEO4J_PASSWORD", "p")
	t.Setenv("VOYAGE_API_KEY", "v")

	_, err := LoadFromEnv()
	if !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("expected ErrMissingRequired, got %v", err)
	}
}

func TestLoadFromEnv_InvalidDimensions(t *testing.T) {
	t.Setenv("MINDGRAPH_API_KEY", "k")
	t.Setenv("NEO4J_URI", "x")
	t.Setenv("NEO4J_USER", "u")
	t.Setenv("NEO4J_PASSWORD", "p")
	t.Setenv("VOYAGE_API_KEY", "v")
	t.Setenv("EMBEDDING_DIMENSIONS", "not-a-number")

	if _, err := LoadFromEnv(); err == nil {
		t.Fatal("expected error for non-numeric EMBEDDING_DIMENSIONS")
	}
}
