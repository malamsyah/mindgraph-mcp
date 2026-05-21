package memory

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/testcontainers/testcontainers-go"
	tcneo4j "github.com/testcontainers/testcontainers-go/modules/neo4j"
)

// Package-level Neo4j container shared across all integration tests.
// Each test still calls cleanupGraph to start from a clean slate.
var (
	sharedRepo      *Repository
	sharedContainer *tcneo4j.Neo4jContainer
)

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tcneo4j.Run(ctx, "neo4j:5.20",
		tcneo4j.WithAdminPassword("testpassword"),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start neo4j: %v\n", err)
		os.Exit(1)
	}
	sharedContainer = container

	uri, err := container.BoltUrl(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bolt url: %v\n", err)
		_ = testcontainers.TerminateContainer(container)
		os.Exit(1)
	}
	repo, err := NewRepository(ctx, uri, "neo4j", "testpassword")
	if err != nil {
		fmt.Fprintf(os.Stderr, "new repo: %v\n", err)
		_ = testcontainers.TerminateContainer(container)
		os.Exit(1)
	}
	sharedRepo = repo
	if err := repo.Bootstrap(ctx, 16); err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap: %v\n", err)
		_ = repo.Close(context.Background())
		_ = testcontainers.TerminateContainer(container)
		os.Exit(1)
	}

	code := m.Run()

	_ = repo.Close(context.Background())
	_ = testcontainers.TerminateContainer(container)
	os.Exit(code)
}

func testRepo(t *testing.T) *Repository {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping testcontainers-based test in short mode")
	}
	if sharedRepo == nil {
		t.Fatal("sharedRepo not initialized (TestMain failed?)")
	}
	cleanupGraph(t, sharedRepo)
	return sharedRepo
}

func cleanupGraph(t *testing.T, repo *Repository) {
	t.Helper()
	_, err := neo4j.ExecuteQuery(context.Background(), repo.driver,
		`MATCH (n) DETACH DELETE n`, nil, neo4j.EagerResultTransformer)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

func TestBootstrap_IsIdempotent(t *testing.T) {
	repo := testRepo(t)
	if err := repo.Bootstrap(context.Background(), 16); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
}

func TestAddMemory_PersistsAndNormalizesTags(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	mem, err := repo.AddMemory(ctx, "first note", []string{"Foo", "  bar  ", "foo"}, nil)
	if err != nil {
		t.Fatalf("AddMemory: %v", err)
	}
	if mem.ID == "" {
		t.Errorf("expected non-empty id")
	}
	if mem.Content != "first note" {
		t.Errorf("content = %q", mem.Content)
	}
	if mem.CreatedAt.IsZero() || mem.UpdatedAt.IsZero() {
		t.Errorf("timestamps not set: %+v", mem)
	}

	detail, err := repo.GetMemory(ctx, mem.ID)
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	gotTags := map[string]bool{}
	for _, t := range detail.Tags {
		gotTags[t] = true
	}
	if !gotTags["foo"] || !gotTags["bar"] {
		t.Errorf("expected tags {foo, bar}, got %v", detail.Tags)
	}
	if len(detail.Tags) != 2 {
		t.Errorf("expected 2 tags, got %d (%v)", len(detail.Tags), detail.Tags)
	}
}

func TestAddMemory_RejectsEmptyContent(t *testing.T) {
	repo := testRepo(t)
	if _, err := repo.AddMemory(context.Background(), "", nil, nil); !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("expected ErrInvalidArgs, got %v", err)
	}
}

func TestGetMemory_NotFound(t *testing.T) {
	repo := testRepo(t)
	_, err := repo.GetMemory(context.Background(), "nope")
	if !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("expected ErrMemoryNotFound, got %v", err)
	}
}

func TestGetMemory_ReturnsIncomingAndOutgoing(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	a := mustAdd(t, repo, ctx, "A", nil, nil)
	b := mustAdd(t, repo, ctx, "B", nil, nil)
	c := mustAdd(t, repo, ctx, "C", nil, nil)

	if err := repo.LinkMemories(ctx, a.ID, b.ID, "refines"); err != nil {
		t.Fatalf("link a->b: %v", err)
	}
	if err := repo.LinkMemories(ctx, c.ID, a.ID, "context-for"); err != nil {
		t.Fatalf("link c->a: %v", err)
	}

	detail, err := repo.GetMemory(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if len(detail.Outgoing) != 1 || detail.Outgoing[0].ID != b.ID || detail.Outgoing[0].Relationship != "refines" {
		t.Errorf("outgoing = %+v", detail.Outgoing)
	}
	if len(detail.Incoming) != 1 || detail.Incoming[0].ID != c.ID || detail.Incoming[0].Relationship != "context-for" {
		t.Errorf("incoming = %+v", detail.Incoming)
	}
}

func TestLinkMemories_Idempotent(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()
	a := mustAdd(t, repo, ctx, "A", nil, nil)
	b := mustAdd(t, repo, ctx, "B", nil, nil)

	if err := repo.LinkMemories(ctx, a.ID, b.ID, "refines"); err != nil {
		t.Fatalf("first link: %v", err)
	}
	if err := repo.LinkMemories(ctx, a.ID, b.ID, "refines"); err != nil {
		t.Fatalf("second link: %v", err)
	}

	detail, _ := repo.GetMemory(ctx, a.ID)
	count := 0
	for _, r := range detail.Outgoing {
		if r.ID == b.ID && r.Relationship == "refines" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 'refines' edge after duplicate link, got %d", count)
	}
}

func TestLinkMemories_ParallelEdgesOnDifferentLabel(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()
	a := mustAdd(t, repo, ctx, "A", nil, nil)
	b := mustAdd(t, repo, ctx, "B", nil, nil)

	_ = repo.LinkMemories(ctx, a.ID, b.ID, "refines")
	_ = repo.LinkMemories(ctx, a.ID, b.ID, "contradicts")

	detail, _ := repo.GetMemory(ctx, a.ID)
	rels := map[string]bool{}
	for _, r := range detail.Outgoing {
		if r.ID == b.ID {
			rels[r.Relationship] = true
		}
	}
	if !rels["refines"] || !rels["contradicts"] {
		t.Errorf("expected parallel edges, got %v", rels)
	}
}

func TestLinkMemories_MissingEndpoint(t *testing.T) {
	repo := testRepo(t)
	a, _ := repo.AddMemory(context.Background(), "A", nil, nil)
	if err := repo.LinkMemories(context.Background(), a.ID, "nope", "refines"); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("expected ErrMemoryNotFound, got %v", err)
	}
}

func TestListRecent_OrdersByUpdatedDesc(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()
	a, _ := repo.AddMemory(ctx, "first", nil, nil)
	time.Sleep(30 * time.Millisecond)
	b, _ := repo.AddMemory(ctx, "second", nil, nil)

	hits, err := repo.ListRecent(ctx, nil, 10)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}
	if hits[0].ID != b.ID || hits[1].ID != a.ID {
		t.Errorf("order = %s, %s; want %s, %s", hits[0].ID, hits[1].ID, b.ID, a.ID)
	}
}

func TestListRecent_TagFilter(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()
	_, _ = repo.AddMemory(ctx, "untagged", nil, nil)
	tagged, _ := repo.AddMemory(ctx, "tagged", []string{"x"}, nil)

	hits, err := repo.ListRecent(ctx, []string{"x"}, 10)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != tagged.ID {
		t.Errorf("expected only tagged, got %+v", hits)
	}
}

func TestSearchFulltext_RanksAndFilters(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()
	_, _ = repo.AddMemory(ctx, "alpha bravo charlie", []string{"x"}, nil)
	_, _ = repo.AddMemory(ctx, "alpha alpha alpha", []string{"y"}, nil)
	_, _ = repo.AddMemory(ctx, "delta echo", []string{"x"}, nil)

	// Need a couple seconds for fulltext index to catch up.
	time.Sleep(2 * time.Second)

	hits, err := repo.SearchFulltext(ctx, "alpha", nil, 10)
	if err != nil {
		t.Fatalf("SearchFulltext: %v", err)
	}
	if len(hits) < 2 {
		t.Fatalf("expected at least 2 hits, got %d", len(hits))
	}
	if hits[0].Score < hits[1].Score {
		t.Errorf("scores not descending: %v", hits)
	}

	// Tag filter narrows the result set.
	hits, err = repo.SearchFulltext(ctx, "alpha", []string{"x"}, 10)
	if err != nil {
		t.Fatalf("SearchFulltext tagged: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("expected 1 tagged result, got %d (%v)", len(hits), hits)
	}
}

func TestSearchFulltext_EmptyQuery(t *testing.T) {
	repo := testRepo(t)
	hits, err := repo.SearchFulltext(context.Background(), "", nil, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("expected empty hits, got %d", len(hits))
	}
}

func TestFindPath_ReturnsShortest(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()
	a := mustAdd(t, repo, ctx, "A", nil, nil)
	b := mustAdd(t, repo, ctx, "B", nil, nil)
	c := mustAdd(t, repo, ctx, "C", nil, nil)
	d := mustAdd(t, repo, ctx, "D", nil, nil)

	_ = repo.LinkMemories(ctx, a.ID, b.ID, "refines")
	_ = repo.LinkMemories(ctx, b.ID, c.ID, "refines")
	_ = repo.LinkMemories(ctx, c.ID, d.ID, "refines")
	_ = repo.LinkMemories(ctx, a.ID, d.ID, "follows-up") // shortcut

	pr, err := repo.FindPath(ctx, a.ID, d.ID, 4)
	if err != nil {
		t.Fatalf("FindPath: %v", err)
	}
	if pr.Hops != 1 {
		t.Errorf("expected hops=1 via shortcut, got %d", pr.Hops)
	}
	if len(pr.Nodes) != 2 {
		t.Errorf("expected 2 nodes, got %d", len(pr.Nodes))
	}
}

func TestFindPath_NoPath(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()
	a := mustAdd(t, repo, ctx, "A", nil, nil)
	b := mustAdd(t, repo, ctx, "B", nil, nil)

	pr, err := repo.FindPath(ctx, a.ID, b.ID, 4)
	if err != nil {
		t.Fatalf("FindPath: %v", err)
	}
	if pr.Hops != -1 {
		t.Errorf("expected hops=-1, got %d", pr.Hops)
	}
}

func TestFindPath_MissingEndpoint(t *testing.T) {
	repo := testRepo(t)
	a, _ := repo.AddMemory(context.Background(), "A", nil, nil)
	_, err := repo.FindPath(context.Background(), a.ID, "nope", 4)
	if !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("expected ErrMemoryNotFound, got %v", err)
	}
}

func TestFindPath_InvalidHops(t *testing.T) {
	repo := testRepo(t)
	if _, err := repo.FindPath(context.Background(), "a", "b", 0); !errors.Is(err, ErrInvalidArgs) {
		t.Errorf("expected ErrInvalidArgs for max_hops=0, got %v", err)
	}
	if _, err := repo.FindPath(context.Background(), "a", "b", 7); !errors.Is(err, ErrInvalidArgs) {
		t.Errorf("expected ErrInvalidArgs for max_hops=7, got %v", err)
	}
}

func TestFindRelated_SortsByDistance(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()
	a := mustAdd(t, repo, ctx, "A", nil, nil)
	b := mustAdd(t, repo, ctx, "B", nil, nil)
	c := mustAdd(t, repo, ctx, "C", nil, nil)

	_ = repo.LinkMemories(ctx, a.ID, b.ID, "refines") // dist 1
	_ = repo.LinkMemories(ctx, b.ID, c.ID, "refines") // dist 2

	res, err := repo.FindRelated(ctx, a.ID, 4, nil, 10)
	if err != nil {
		t.Fatalf("FindRelated: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 related, got %d (%+v)", len(res), res)
	}
	if res[0].Distance != 1 || res[1].Distance != 2 {
		t.Errorf("expected distances 1,2, got %d,%d", res[0].Distance, res[1].Distance)
	}
}

func TestFindRelated_RelationshipFilter(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()
	a := mustAdd(t, repo, ctx, "A", nil, nil)
	b := mustAdd(t, repo, ctx, "B", nil, nil)
	c := mustAdd(t, repo, ctx, "C", nil, nil)

	_ = repo.LinkMemories(ctx, a.ID, b.ID, "refines")
	_ = repo.LinkMemories(ctx, a.ID, c.ID, "contradicts")

	res, err := repo.FindRelated(ctx, a.ID, 2, []string{"refines"}, 10)
	if err != nil {
		t.Fatalf("FindRelated: %v", err)
	}
	if len(res) != 1 || res[0].ID != b.ID {
		t.Errorf("expected only B, got %+v", res)
	}
}

func TestFindRelated_MissingStart(t *testing.T) {
	repo := testRepo(t)
	_, err := repo.FindRelated(context.Background(), "nope", 2, nil, 10)
	if !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("expected ErrMemoryNotFound, got %v", err)
	}
}

func TestSearchSemantic_FiltersAndOrdersByScore(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	// 16-dim embeddings to match Bootstrap's test dim.
	v1 := normalizedVec(16, 0)
	v2 := normalizedVec(16, 1)
	v3 := normalizedVec(16, 2)

	a, _ := repo.AddMemory(ctx, "memA", []string{"x"}, v1)
	b, _ := repo.AddMemory(ctx, "memB", []string{"x"}, v2)
	_, _ = repo.AddMemory(ctx, "memC", []string{"y"}, v3)

	// Query identical to v1 ⇒ a should rank first.
	hits, err := repo.SearchSemantic(ctx, v1, nil, 3)
	if err != nil {
		t.Fatalf("SearchSemantic: %v", err)
	}
	if len(hits) == 0 || hits[0].ID != a.ID {
		t.Errorf("expected first hit to be %s, got %+v", a.ID, hits)
	}

	// Tag filter excludes c. Query nearer to v2; ensure b returned.
	hits, err = repo.SearchSemantic(ctx, v2, []string{"x"}, 3)
	if err != nil {
		t.Fatalf("SearchSemantic with tag: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.ID == b.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("expected b in tagged results, got %+v", hits)
	}
}

func TestMissingEmbeddingsAndUpdate(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()
	m, _ := repo.AddMemory(ctx, "no embedding", nil, nil)
	missing, err := repo.MissingEmbeddings(ctx, 10)
	if err != nil {
		t.Fatalf("MissingEmbeddings: %v", err)
	}
	if len(missing) != 1 || missing[0].ID != m.ID {
		t.Fatalf("expected 1 missing = %s, got %+v", m.ID, missing)
	}
	if err := repo.UpdateEmbedding(ctx, m.ID, normalizedVec(16, 0)); err != nil {
		t.Fatalf("UpdateEmbedding: %v", err)
	}
	missing, _ = repo.MissingEmbeddings(ctx, 10)
	if len(missing) != 0 {
		t.Errorf("expected 0 missing after update, got %d", len(missing))
	}
}

func TestListAllForReembed_PaginatesInIDOrder(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()

	// UUIDv7 ids are time-ordered; AddMemory in sequence yields monotonically
	// increasing ids, so afterID-based pagination should walk creation order.
	a := mustAdd(t, repo, ctx, "first", nil, normalizedVec(16, 0))
	b := mustAdd(t, repo, ctx, "second", nil, normalizedVec(16, 1))
	c, _ := repo.AddMemory(ctx, "third with null embedding", nil, nil)

	first, err := repo.ListAllForReembed(ctx, "", 2)
	if err != nil {
		t.Fatalf("ListAllForReembed page 1: %v", err)
	}
	if len(first) != 2 || first[0].ID != a.ID || first[1].ID != b.ID {
		t.Fatalf("page 1 = %+v, want [a b]", ids(first))
	}

	next, err := repo.ListAllForReembed(ctx, first[len(first)-1].ID, 2)
	if err != nil {
		t.Fatalf("ListAllForReembed page 2: %v", err)
	}
	// scope=all returns memories regardless of embedding state, so c is included.
	if len(next) != 1 || next[0].ID != c.ID {
		t.Errorf("page 2 = %+v, want [c]", ids(next))
	}

	empty, err := repo.ListAllForReembed(ctx, next[len(next)-1].ID, 2)
	if err != nil {
		t.Fatalf("ListAllForReembed page 3: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("page 3 = %+v, want []", ids(empty))
	}
}

func TestGetMemoryContent(t *testing.T) {
	repo := testRepo(t)
	ctx := context.Background()
	m := mustAdd(t, repo, ctx, "hello world", nil, nil)

	got, err := repo.GetMemoryContent(ctx, m.ID)
	if err != nil {
		t.Fatalf("GetMemoryContent: %v", err)
	}
	if got != "hello world" {
		t.Errorf("content = %q, want %q", got, "hello world")
	}

	if _, err := repo.GetMemoryContent(ctx, "missing"); !errors.Is(err, ErrMemoryNotFound) {
		t.Errorf("expected ErrMemoryNotFound for unknown id, got %v", err)
	}
}

func ids(ms []Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

// normalizedVec returns a unit vector of dim N with a 1.0 at index hot.
func normalizedVec(dim, hot int) []float32 {
	v := make([]float32, dim)
	v[hot] = 1
	return v
}

func mustAdd(t *testing.T, repo *Repository, ctx context.Context, content string, tags []string, emb []float32) *Memory {
	t.Helper()
	m, err := repo.AddMemory(ctx, content, tags, emb)
	if err != nil {
		t.Fatalf("AddMemory(%q): %v", content, err)
	}
	return m
}
