// Package reembed regenerates embeddings for memories that are missing one
// (e.g., from a Voyage outage during add_memory) or for the entire corpus
// (e.g., recovering from a batch of bad embeddings, or after switching models).
//
// It is shared by the boot-time backfill in cmd/server/main.go and the
// reembed_memories MCP tool, so both code paths report progress the same way.
package reembed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/malamsyah/mindgraph-mcp/internal/embeddings"
	"github.com/malamsyah/mindgraph-mcp/internal/memory"
)

type Scope string

const (
	ScopeMissing Scope = "missing"
	ScopeAll     Scope = "all"
)

// DefaultBatch matches the original boot backfill batch size; sized to keep
// Voyage AI request payloads well under their per-call token limit.
const DefaultBatch = 128

// Options controls one invocation of Run.
//
// Either ID xor Scope is meaningful: if ID is non-empty, only that memory is
// re-embedded and Scope is ignored. Otherwise Scope selects the target set.
type Options struct {
	Scope     Scope
	ID        string
	Max       int // <=0 means no upper bound
	BatchSize int // <=0 uses DefaultBatch
}

// Result is the summary returned by Run. Failures carries per-id error
// messages for memories that failed to re-embed; succeeded + failed always
// equals processed.
type Result struct {
	Scope     Scope     `json:"scope,omitempty"`
	ID        string    `json:"id,omitempty"`
	Processed int       `json:"processed"`
	Succeeded int       `json:"succeeded"`
	Failed    int       `json:"failed"`
	Failures  []Failure `json:"failures,omitempty"`
}

type Failure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// Repo is the subset of memory.Repository required by Run; declared here so
// tests can supply a fake without spinning up Neo4j.
type Repo interface {
	MissingEmbeddings(ctx context.Context, limit int) ([]memory.Memory, error)
	ListAllForReembed(ctx context.Context, afterID string, limit int) ([]memory.Memory, error)
	GetMemoryContent(ctx context.Context, id string) (string, error)
	UpdateEmbedding(ctx context.Context, id string, vec []float32) error
}

// Run executes a re-embed pass per opts. Returns a partial Result on error so
// callers can report what got done before the failure.
func Run(ctx context.Context, repo Repo, embedder embeddings.Embedder, opts Options) (*Result, error) {
	if embedder == nil {
		return nil, errors.New("reembed: embedder is required")
	}

	if opts.ID != "" {
		return runOne(ctx, repo, embedder, opts.ID)
	}

	if opts.Scope == "" {
		opts.Scope = ScopeMissing
	}
	if opts.Scope != ScopeMissing && opts.Scope != ScopeAll {
		return nil, fmt.Errorf("reembed: unknown scope %q", opts.Scope)
	}

	batch := opts.BatchSize
	if batch <= 0 {
		batch = DefaultBatch
	}

	result := &Result{Scope: opts.Scope}
	afterID := ""

	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		n := batch
		if opts.Max > 0 {
			remaining := opts.Max - result.Processed
			if remaining <= 0 {
				return result, nil
			}
			if remaining < n {
				n = remaining
			}
		}

		pending, err := fetchBatch(ctx, repo, opts.Scope, afterID, n)
		if err != nil {
			return result, fmt.Errorf("fetch batch: %w", err)
		}
		if len(pending) == 0 {
			return result, nil
		}

		texts := make([]string, len(pending))
		for i, m := range pending {
			texts[i] = m.Content
		}

		vecs, embErr := embedder.Embed(ctx, texts, embeddings.InputDocument)
		if embErr != nil {
			// Whole batch failed at the embedder. Record per-id failures so
			// the caller sees which IDs were affected, then bail out: hammering
			// Voyage harder won't help.
			for _, m := range pending {
				result.Processed++
				result.Failed++
				result.Failures = append(result.Failures, Failure{ID: m.ID, Error: embErr.Error()})
			}
			return result, embErr
		}

		batchSucceeded := 0
		for i, m := range pending {
			result.Processed++
			if i >= len(vecs) || len(vecs[i]) == 0 {
				result.Failed++
				result.Failures = append(result.Failures, Failure{ID: m.ID, Error: "no vector returned"})
				continue
			}
			if err := repo.UpdateEmbedding(ctx, m.ID, vecs[i]); err != nil {
				slog.Warn("reembed: update embedding failed", "id", m.ID, "err", err)
				result.Failed++
				result.Failures = append(result.Failures, Failure{ID: m.ID, Error: err.Error()})
				continue
			}
			result.Succeeded++
			batchSucceeded++
		}

		if opts.Scope == ScopeAll {
			// id-cursor pagination over all memories (UUIDv7 = time-ordered).
			afterID = pending[len(pending)-1].ID
		} else if batchSucceeded == 0 {
			// scope=missing relies on rows leaving the NULL pool to advance.
			// If a full batch made zero progress, every UpdateEmbedding errored
			// — likely a persistent DB problem; stop instead of spinning.
			return result, errors.New("reembed: missing-scope batch made no progress")
		}

		if len(pending) < n {
			return result, nil
		}
	}
}

func runOne(ctx context.Context, repo Repo, embedder embeddings.Embedder, id string) (*Result, error) {
	result := &Result{ID: id, Processed: 1}
	content, err := repo.GetMemoryContent(ctx, id)
	if err != nil {
		return nil, err
	}
	vecs, err := embedder.Embed(ctx, []string{content}, embeddings.InputDocument)
	if err != nil || len(vecs) == 0 || len(vecs[0]) == 0 {
		msg := "no vector returned"
		if err != nil {
			msg = err.Error()
		}
		result.Failed = 1
		result.Failures = []Failure{{ID: id, Error: msg}}
		if err == nil {
			err = errors.New(msg)
		}
		return result, err
	}
	if err := repo.UpdateEmbedding(ctx, id, vecs[0]); err != nil {
		result.Failed = 1
		result.Failures = []Failure{{ID: id, Error: err.Error()}}
		return result, err
	}
	result.Succeeded = 1
	return result, nil
}

func fetchBatch(ctx context.Context, repo Repo, scope Scope, afterID string, limit int) ([]memory.Memory, error) {
	switch scope {
	case ScopeMissing:
		return repo.MissingEmbeddings(ctx, limit)
	case ScopeAll:
		return repo.ListAllForReembed(ctx, afterID, limit)
	default:
		return nil, fmt.Errorf("unknown scope %q", scope)
	}
}
