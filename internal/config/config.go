package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	APIKey                string
	Neo4jURI              string
	Neo4jUser             string
	Neo4jPassword         string
	VoyageAPIKey          string
	EmbeddingModel        string
	EmbeddingDimensions   int
	Port                  string
	LogLevel              string
	SuggestLinksThreshold float64
}

var ErrMissingRequired = errors.New("missing required environment variable")

func LoadFromEnv() (*Config, error) {
	cfg := &Config{
		APIKey:         os.Getenv("MINDGRAPH_API_KEY"),
		Neo4jURI:       os.Getenv("NEO4J_URI"),
		Neo4jUser:      os.Getenv("NEO4J_USER"),
		Neo4jPassword:  os.Getenv("NEO4J_PASSWORD"),
		VoyageAPIKey:   os.Getenv("VOYAGE_API_KEY"),
		EmbeddingModel: getenv("EMBEDDING_MODEL", "voyage-3-large"),
		Port:           getenv("PORT", "8080"),
		LogLevel:       getenv("LOG_LEVEL", "info"),
	}

	dims, err := strconv.Atoi(getenv("EMBEDDING_DIMENSIONS", "2048"))
	if err != nil {
		return nil, fmt.Errorf("EMBEDDING_DIMENSIONS must be an integer: %w", err)
	}
	cfg.EmbeddingDimensions = dims

	threshold, err := strconv.ParseFloat(getenv("SUGGEST_LINKS_THRESHOLD", "0.75"), 64)
	if err != nil {
		return nil, fmt.Errorf("SUGGEST_LINKS_THRESHOLD must be a float: %w", err)
	}
	cfg.SuggestLinksThreshold = threshold

	required := map[string]string{
		"MINDGRAPH_API_KEY": cfg.APIKey,
		"NEO4J_URI":         cfg.Neo4jURI,
		"NEO4J_USER":        cfg.Neo4jUser,
		"NEO4J_PASSWORD":    cfg.Neo4jPassword,
		"VOYAGE_API_KEY":    cfg.VoyageAPIKey,
	}
	for name, value := range required {
		if value == "" {
			return nil, fmt.Errorf("%w: %s", ErrMissingRequired, name)
		}
	}

	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
