package reembed

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/malamsyah/mindgraph-mcp/internal/embeddings"
	"github.com/malamsyah/mindgraph-mcp/internal/memory"
)

// fakeRepo backs the Repo interface for unit tests. It stores memories keyed by
// id with an `embedded` flag flipped by UpdateEmbedding, so the missing-scope
// path observes rows leaving the NULL pool the same way the real DB does.
type fakeRepo struct {
	items        []memory.Memory
	embedded     map[string]bool
	updateErr    map[string]error
	missingCalls int
	allCalls     int
}

func newFakeRepo(items []memory.Memory) *fakeRepo {
	return &fakeRepo{items: items, embedded: map[string]bool{}}
}

func (f *fakeRepo) MissingEmbeddings(_ context.Context, limit int) ([]memory.Memory, error) {
	f.missingCalls++
	out := []memory.Memory{}
	for _, m := range f.items {
		if !f.embedded[m.ID] {
			out = append(out, m)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeRepo) ListAllForReembed(_ context.Context, afterID string, limit int) ([]memory.Memory, error) {
	f.allCalls++
	out := []memory.Memory{}
	for _, m := range f.items {
		if m.ID <= afterID {
			continue
		}
		out = append(out, m)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeRepo) GetMemoryContent(_ context.Context, id string) (string, error) {
	for _, m := range f.items {
		if m.ID == id {
			return m.Content, nil
		}
	}
	return "", memory.ErrMemoryNotFound
}

func (f *fakeRepo) UpdateEmbedding(_ context.Context, id string, _ []float32) error {
	if err, ok := f.updateErr[id]; ok {
		return err
	}
	f.embedded[id] = true
	return nil
}

// fakeEmbedder returns a deterministic single-element vector per input, with a
// configurable per-call error and a counter for assertions.
type fakeEmbedder struct {
	err   error
	calls atomic.Int32
}

func (e *fakeEmbedder) Embed(_ context.Context, texts []string, _ embeddings.InputType) ([][]float32, error) {
	e.calls.Add(1)
	if e.err != nil {
		return nil, e.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{float32(i + 1)}
	}
	return out, nil
}

func makeMemories(ids ...string) []memory.Memory {
	out := make([]memory.Memory, len(ids))
	for i, id := range ids {
		out[i] = memory.Memory{ID: id, Content: "c-" + id}
	}
	return out
}

func TestRun_NilEmbedderRejected(t *testing.T) {
	repo := newFakeRepo(nil)
	if _, err := Run(context.Background(), repo, nil, Options{}); err == nil {
		t.Fatal("expected error when embedder is nil")
	}
}

func TestRun_MissingScopeEmbedsOnlyNull(t *testing.T) {
	repo := newFakeRepo(makeMemories("a", "b", "c"))
	repo.embedded["b"] = true // b already has an embedding
	emb := &fakeEmbedder{}

	res, err := Run(context.Background(), repo, emb, Options{Scope: ScopeMissing, BatchSize: 10})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Processed != 2 || res.Succeeded != 2 || res.Failed != 0 {
		t.Errorf("got %+v, want processed=2 succeeded=2 failed=0", res)
	}
	if !repo.embedded["a"] || !repo.embedded["c"] {
		t.Errorf("expected a and c embedded; embedded=%v", repo.embedded)
	}
}

func TestRun_AllScopePaginatesAcrossBatches(t *testing.T) {
	repo := newFakeRepo(makeMemories("a", "b", "c", "d", "e"))
	emb := &fakeEmbedder{}

	res, err := Run(context.Background(), repo, emb, Options{Scope: ScopeAll, BatchSize: 2})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Processed != 5 || res.Succeeded != 5 {
		t.Errorf("got %+v, want processed=5 succeeded=5", res)
	}
	// 5 items @ batch=2 ⇒ 3 list calls (2,2,1).
	if repo.allCalls != 3 {
		t.Errorf("allCalls = %d, want 3", repo.allCalls)
	}
}

func TestRun_AllScopeRespectsMaxCap(t *testing.T) {
	repo := newFakeRepo(makeMemories("a", "b", "c", "d", "e"))
	emb := &fakeEmbedder{}

	res, err := Run(context.Background(), repo, emb, Options{Scope: ScopeAll, BatchSize: 10, Max: 3})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Processed != 3 {
		t.Errorf("processed = %d, want 3 (capped by Max)", res.Processed)
	}
}

func TestRun_EmbedderErrorRecordsBatchFailures(t *testing.T) {
	repo := newFakeRepo(makeMemories("a", "b"))
	emb := &fakeEmbedder{err: errors.New("voyage down")}

	res, err := Run(context.Background(), repo, emb, Options{Scope: ScopeMissing, BatchSize: 10})
	if err == nil {
		t.Fatal("expected error from embedder")
	}
	if res.Processed != 2 || res.Failed != 2 || res.Succeeded != 0 {
		t.Errorf("got %+v, want processed=2 failed=2", res)
	}
	if len(res.Failures) != 2 {
		t.Errorf("expected 2 failure entries, got %d", len(res.Failures))
	}
}

func TestRun_MissingScopeNoProgressBailsOut(t *testing.T) {
	repo := newFakeRepo(makeMemories("a"))
	repo.updateErr = map[string]error{"a": errors.New("write conflict")}
	emb := &fakeEmbedder{}

	res, err := Run(context.Background(), repo, emb, Options{Scope: ScopeMissing, BatchSize: 10})
	if err == nil {
		t.Fatal("expected error from no-progress detection")
	}
	if res.Succeeded != 0 || res.Failed != 1 {
		t.Errorf("got %+v, want succeeded=0 failed=1", res)
	}
}

func TestRun_SingleIDEmbedsThatMemory(t *testing.T) {
	repo := newFakeRepo(makeMemories("a", "b"))
	emb := &fakeEmbedder{}

	res, err := Run(context.Background(), repo, emb, Options{ID: "b"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ID != "b" || res.Succeeded != 1 || res.Processed != 1 {
		t.Errorf("got %+v", res)
	}
	if repo.embedded["a"] {
		t.Error("expected a NOT to be embedded")
	}
	if !repo.embedded["b"] {
		t.Error("expected b to be embedded")
	}
}

func TestRun_SingleIDMissingReturnsNotFound(t *testing.T) {
	repo := newFakeRepo(makeMemories("a"))
	emb := &fakeEmbedder{}

	_, err := Run(context.Background(), repo, emb, Options{ID: "ghost"})
	if !errors.Is(err, memory.ErrMemoryNotFound) {
		t.Fatalf("expected ErrMemoryNotFound, got %v", err)
	}
}

func TestRun_UnknownScopeRejected(t *testing.T) {
	repo := newFakeRepo(nil)
	emb := &fakeEmbedder{}
	if _, err := Run(context.Background(), repo, emb, Options{Scope: "weird"}); err == nil {
		t.Fatal("expected error for unknown scope")
	}
}

func TestRun_EmptyResultSetIsClean(t *testing.T) {
	repo := newFakeRepo(nil)
	emb := &fakeEmbedder{}

	res, err := Run(context.Background(), repo, emb, Options{Scope: ScopeMissing})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Processed != 0 || emb.calls.Load() != 0 {
		t.Errorf("expected no work; res=%+v calls=%d", res, emb.calls.Load())
	}
}
