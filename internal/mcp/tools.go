package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/malamsyah/mindgraph-mcp/internal/embeddings"
	"github.com/malamsyah/mindgraph-mcp/internal/memory"
	"github.com/malamsyah/mindgraph-mcp/internal/reembed"
	"github.com/malamsyah/mindgraph-mcp/internal/search"
	"golang.org/x/sync/errgroup"
)

// Handlers binds the dependencies needed by every MCP tool.
//
// Embedder may be nil during Phase 1; semantic and hybrid search modes return
// InvalidArgument until it is wired up.
type Handlers struct {
	Repo     *memory.Repository
	Embedder embeddings.Embedder
}

func NewHandlers(repo *memory.Repository, embedder embeddings.Embedder) *Handlers {
	return &Handlers{Repo: repo, Embedder: embedder}
}

// Register attaches all mindgraph tools to the MCP server.
func (h *Handlers) Register(s *server.MCPServer) {
	s.AddTool(addMemoryTool(), h.handleAddMemory)
	s.AddTool(searchMemoryTool(), h.handleSearchMemory)
	s.AddTool(getMemoryTool(), h.handleGetMemory)
	s.AddTool(linkMemoriesTool(), h.handleLinkMemories)
	s.AddTool(listRecentTool(), h.handleListRecent)
	s.AddTool(findPathTool(), h.handleFindPath)
	s.AddTool(findRelatedTool(), h.handleFindRelated)
	s.AddTool(reembedMemoriesTool(), h.handleReembedMemories)
	s.AddTool(deleteMemoryTool(), h.handleDeleteMemory)
	s.AddTool(deleteTagTool(), h.handleDeleteTag)
	s.AddTool(updateTagTool(), h.handleUpdateTag)
	s.AddTool(mergeTagsTool(), h.handleMergeTags)
}

// ---- tool definitions ----

func addMemoryTool() mcp.Tool {
	return mcp.NewTool("add_memory",
		mcp.WithDescription("Create a new memory with optional tags. Embedding is generated automatically when configured."),
		mcp.WithString("content", mcp.Required(), mcp.MinLength(1),
			mcp.Description("Markdown content of the memory.")),
		mcp.WithArray("tags",
			mcp.Description("Optional tag list; lowercased and de-duplicated."),
			mcp.WithStringItems()),
	)
}

func searchMemoryTool() mcp.Tool {
	return mcp.NewTool("search_memory",
		mcp.WithDescription("Search memories by fulltext, semantic, or hybrid mode."),
		mcp.WithString("query", mcp.Description("Search query string.")),
		mcp.WithString("mode",
			mcp.Description("Search mode: fulltext, semantic, or hybrid (default)."),
			mcp.Enum("fulltext", "semantic", "hybrid"),
			mcp.DefaultString("hybrid")),
		mcp.WithArray("tags",
			mcp.Description("Optional tag filter (AND across tags)."),
			mcp.WithStringItems()),
		mcp.WithNumber("limit",
			mcp.Description("Max results (1-50, default 10)."),
			mcp.Min(1), mcp.Max(50), mcp.DefaultNumber(10)),
	)
}

func getMemoryTool() mcp.Tool {
	return mcp.NewTool("get_memory",
		mcp.WithDescription("Fetch a memory by ID with its tags and relationships."),
		mcp.WithString("id", mcp.Required(), mcp.Description("Memory UUIDv7.")),
	)
}

func linkMemoriesTool() mcp.Tool {
	return mcp.NewTool("link_memories",
		mcp.WithDescription("Create or update a typed relationship between two memories."),
		mcp.WithString("from_id", mcp.Required()),
		mcp.WithString("to_id", mcp.Required()),
		mcp.WithString("relationship", mcp.Required(), mcp.MinLength(1),
			mcp.Description("Relationship label, e.g. 'refines', 'contradicts'.")),
	)
}

func listRecentTool() mcp.Tool {
	return mcp.NewTool("list_recent",
		mcp.WithDescription("Most recently updated memories, optionally tag-filtered."),
		mcp.WithArray("tags",
			mcp.Description("Optional tag filter (AND across tags)."),
			mcp.WithStringItems()),
		mcp.WithNumber("limit",
			mcp.Description("Max results (default 10)."),
			mcp.Min(1), mcp.Max(50), mcp.DefaultNumber(10)),
	)
}

func findPathTool() mcp.Tool {
	return mcp.NewTool("find_path",
		mcp.WithDescription("Shortest undirected RELATES_TO path between two memories. hops=-1 when no path exists."),
		mcp.WithString("from_id", mcp.Required()),
		mcp.WithString("to_id", mcp.Required()),
		mcp.WithNumber("max_hops",
			mcp.Description("Max path length (1-6, default 4)."),
			mcp.Min(1), mcp.Max(6), mcp.DefaultNumber(4)),
	)
}

func findRelatedTool() mcp.Tool {
	return mcp.NewTool("find_related",
		mcp.WithDescription("Memories within N hops of the starting memory, sorted by graph distance."),
		mcp.WithString("id", mcp.Required()),
		mcp.WithNumber("hops",
			mcp.Description("Max hops (1-4, default 2)."),
			mcp.Min(1), mcp.Max(4), mcp.DefaultNumber(2)),
		mcp.WithArray("relationship_filter",
			mcp.Description("If non-empty, only edges with these labels count."),
			mcp.WithStringItems()),
		mcp.WithNumber("limit",
			mcp.Description("Max results (default 20)."),
			mcp.Min(1), mcp.Max(50), mcp.DefaultNumber(20)),
	)
}

func deleteTagTool() mcp.Tool {
	return mcp.NewTool("delete_tag",
		mcp.WithDescription("Permanently delete a tag and remove it from every memory it's attached to. Memories themselves are preserved. Irreversible."),
		mcp.WithString("name", mcp.Required(), mcp.MinLength(1),
			mcp.Description("Tag name to delete (normalized to lowercase + trim).")),
	)
}

func updateTagTool() mcp.Tool {
	return mcp.NewTool("update_tag",
		mcp.WithDescription("Rename a tag. The new name must not already exist — use merge_tags to combine two existing tags."),
		mcp.WithString("old_name", mcp.Required(), mcp.MinLength(1),
			mcp.Description("Current tag name.")),
		mcp.WithString("new_name", mcp.Required(), mcp.MinLength(1),
			mcp.Description("New tag name.")),
	)
}

func mergeTagsTool() mcp.Tool {
	return mcp.NewTool("merge_tags",
		mcp.WithDescription("Fold source tag into target tag: every memory tagged with source becomes tagged with target (no duplicate edges), then source is deleted. Both tags must already exist."),
		mcp.WithString("source", mcp.Required(), mcp.MinLength(1),
			mcp.Description("Tag to fold in and remove.")),
		mcp.WithString("target", mcp.Required(), mcp.MinLength(1),
			mcp.Description("Tag to keep; receives source's memories.")),
	)
}

func deleteMemoryTool() mcp.Tool {
	return mcp.NewTool("delete_memory",
		mcp.WithDescription("Permanently delete a memory and all of its relationships. Tag nodes are preserved. This is irreversible — the memory cannot be recovered."),
		mcp.WithString("id", mcp.Required(), mcp.Description("Memory UUIDv7 to delete.")),
	)
}

func reembedMemoriesTool() mcp.Tool {
	return mcp.NewTool("reembed_memories",
		mcp.WithDescription("Regenerate embeddings for memories whose previous embedding call failed (scope=missing, default) or for every memory (scope=all). If id is set, only that memory is re-embedded."),
		mcp.WithString("scope",
			mcp.Description("Which memories to re-embed: 'missing' (NULL embedding only) or 'all'."),
			mcp.Enum("missing", "all"),
			mcp.DefaultString("missing")),
		mcp.WithString("id",
			mcp.Description("Optional. If set, re-embed only this memory and ignore scope.")),
		mcp.WithNumber("limit",
			mcp.Description("Max memories to process in this call. 0 = no cap."),
			mcp.Min(0), mcp.Max(10000), mcp.DefaultNumber(0)),
	)
}

// ---- handlers ----

func (h *Handlers) handleAddMemory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	content, err := req.RequireString("content")
	if err != nil {
		return invalidArg(err.Error()), nil
	}
	tags := req.GetStringSlice("tags", nil)

	var (
		embedding []float32
		warning   string
	)
	if h.Embedder != nil {
		vecs, embErr := h.Embedder.Embed(ctx, []string{content}, embeddings.InputDocument)
		switch {
		case embErr != nil:
			slog.Warn("add_memory embedding failed; persisting with null embedding",
				"err", embErr)
			warning = "embedding_failed"
		case len(vecs) == 0 || len(vecs[0]) == 0:
			// Non-nil-but-empty vectors would otherwise be persisted as a non-NULL
			// empty list, evading reembed_memories(scope=missing)'s IS NULL filter.
			// Treat as a failed embed so the row is recoverable by re-embed.
			slog.Warn("add_memory embedding empty; persisting with null embedding")
			warning = "embedding_failed"
		default:
			embedding = vecs[0]
		}
	}

	mem, err := h.Repo.AddMemory(ctx, content, tags, embedding)
	if err != nil {
		return mapRepoError(err)
	}

	resp := map[string]any{
		"id":         mem.ID,
		"content":    mem.Content,
		"created_at": mem.CreatedAt,
	}
	if warning != "" {
		resp["warning"] = warning
	}
	return jsonResult(resp)
}

func (h *Handlers) handleSearchMemory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := req.GetString("query", "")
	mode := memory.SearchMode(req.GetString("mode", string(memory.SearchHybridMode)))
	tags := req.GetStringSlice("tags", nil)
	limit := req.GetInt("limit", 10)

	switch mode {
	case memory.SearchFulltextMode:
		hits, err := h.Repo.SearchFulltext(ctx, query, tags, limit)
		if err != nil {
			return mapRepoError(err)
		}
		return jsonResult(map[string]any{"mode": mode, "hits": hits})
	case memory.SearchSemanticMode:
		hits, warning, err := h.semanticSearch(ctx, query, tags, limit)
		if err != nil {
			return mapRepoError(err)
		}
		return jsonResult(searchResp(mode, hits, warning))
	case memory.SearchHybridMode:
		hits, warning, err := h.hybridSearch(ctx, query, tags, limit)
		if err != nil {
			return mapRepoError(err)
		}
		return jsonResult(searchResp(mode, hits, warning))
	default:
		return invalidArg(fmt.Sprintf("unknown mode %q", mode)), nil
	}
}

func (h *Handlers) handleGetMemory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return invalidArg(err.Error()), nil
	}
	detail, err := h.Repo.GetMemory(ctx, id)
	if err != nil {
		return mapRepoError(err)
	}
	return jsonResult(detail)
}

func (h *Handlers) handleLinkMemories(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	from, err := req.RequireString("from_id")
	if err != nil {
		return invalidArg(err.Error()), nil
	}
	to, err := req.RequireString("to_id")
	if err != nil {
		return invalidArg(err.Error()), nil
	}
	rel, err := req.RequireString("relationship")
	if err != nil {
		return invalidArg(err.Error()), nil
	}
	if err := h.Repo.LinkMemories(ctx, from, to, rel); err != nil {
		return mapRepoError(err)
	}
	return jsonResult(map[string]any{
		"from_id":      from,
		"to_id":        to,
		"relationship": rel,
	})
}

func (h *Handlers) handleListRecent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	tags := req.GetStringSlice("tags", nil)
	limit := req.GetInt("limit", 10)
	hits, err := h.Repo.ListRecent(ctx, tags, limit)
	if err != nil {
		return mapRepoError(err)
	}
	return jsonResult(map[string]any{"hits": hits})
}

func (h *Handlers) handleFindPath(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	from, err := req.RequireString("from_id")
	if err != nil {
		return invalidArg(err.Error()), nil
	}
	to, err := req.RequireString("to_id")
	if err != nil {
		return invalidArg(err.Error()), nil
	}
	maxHops := req.GetInt("max_hops", 4)
	res, err := h.Repo.FindPath(ctx, from, to, maxHops)
	if err != nil {
		return mapRepoError(err)
	}
	return jsonResult(res)
}

func (h *Handlers) handleFindRelated(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return invalidArg(err.Error()), nil
	}
	hops := req.GetInt("hops", 2)
	limit := req.GetInt("limit", 20)
	filter := req.GetStringSlice("relationship_filter", nil)
	res, err := h.Repo.FindRelated(ctx, id, hops, filter, limit)
	if err != nil {
		return mapRepoError(err)
	}
	return jsonResult(map[string]any{"related": res})
}

func (h *Handlers) handleDeleteTag(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return invalidArg(err.Error()), nil
	}
	affected, err := h.Repo.DeleteTag(ctx, name)
	if err != nil {
		return mapRepoError(err)
	}
	return jsonResult(map[string]any{
		"name":              memory.NormalizeTagName(name),
		"deleted":           true,
		"memories_affected": affected,
	})
}

func (h *Handlers) handleUpdateTag(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	oldName, err := req.RequireString("old_name")
	if err != nil {
		return invalidArg(err.Error()), nil
	}
	newName, err := req.RequireString("new_name")
	if err != nil {
		return invalidArg(err.Error()), nil
	}
	affected, err := h.Repo.RenameTag(ctx, oldName, newName)
	if err != nil {
		return mapRepoError(err)
	}
	return jsonResult(map[string]any{
		"old_name":          memory.NormalizeTagName(oldName),
		"new_name":          memory.NormalizeTagName(newName),
		"renamed":           true,
		"memories_affected": affected,
	})
}

func (h *Handlers) handleMergeTags(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	src, err := req.RequireString("source")
	if err != nil {
		return invalidArg(err.Error()), nil
	}
	dst, err := req.RequireString("target")
	if err != nil {
		return invalidArg(err.Error()), nil
	}
	affected, err := h.Repo.MergeTags(ctx, src, dst)
	if err != nil {
		return mapRepoError(err)
	}
	return jsonResult(map[string]any{
		"source":            memory.NormalizeTagName(src),
		"target":            memory.NormalizeTagName(dst),
		"merged":            true,
		"memories_affected": affected,
	})
}

func (h *Handlers) handleDeleteMemory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("id")
	if err != nil {
		return invalidArg(err.Error()), nil
	}
	if err := h.Repo.DeleteMemory(ctx, id); err != nil {
		return mapRepoError(err)
	}
	return jsonResult(map[string]any{"id": id, "deleted": true})
}

func (h *Handlers) handleReembedMemories(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if h.Embedder == nil {
		return invalidArg("reembed requires embedder configuration"), nil
	}
	opts := reembed.Options{
		Scope: reembed.Scope(req.GetString("scope", string(reembed.ScopeMissing))),
		ID:    req.GetString("id", ""),
		Max:   req.GetInt("limit", 0),
	}
	result, err := reembed.Run(ctx, h.Repo, h.Embedder, opts)
	if err != nil {
		// Surface partial progress alongside the error rather than dropping it.
		if errors.Is(err, memory.ErrMemoryNotFound) {
			return mapRepoError(err)
		}
		slog.Error("reembed failed", "err", err, "processed", processedOr(result))
		if result == nil {
			return mcp.NewToolResultError("Internal: " + err.Error()), nil
		}
		return jsonResult(map[string]any{
			"result": result,
			"error":  err.Error(),
		})
	}
	return jsonResult(result)
}

func processedOr(r *reembed.Result) int {
	if r == nil {
		return 0
	}
	return r.Processed
}

// ---- helpers ----

func (h *Handlers) semanticSearch(ctx context.Context, query string, tags []string, limit int) ([]memory.SearchHit, string, error) {
	if h.Embedder == nil {
		return nil, "", fmt.Errorf("%w: semantic mode requires embedder configuration", memory.ErrInvalidArgs)
	}
	if query == "" {
		return nil, "", nil
	}
	vecs, err := h.Embedder.Embed(ctx, []string{query}, embeddings.InputQuery)
	if err != nil || len(vecs) == 0 {
		// Fall back to fulltext per SPEC §5 failure modes.
		slog.Warn("semantic embed failed; falling back to fulltext", "err", err)
		fb, fbErr := h.Repo.SearchFulltext(ctx, query, tags, limit)
		return fb, "embedding_failed_fallback_fulltext", fbErr
	}
	hits, err := h.Repo.SearchSemantic(ctx, vecs[0], tags, limit)
	return hits, "", err
}

func (h *Handlers) hybridSearch(ctx context.Context, query string, tags []string, limit int) ([]memory.SearchHit, string, error) {
	if h.Embedder == nil {
		// No embedder: degrade to fulltext.
		hits, err := h.Repo.SearchFulltext(ctx, query, tags, limit)
		return hits, "embedder_unavailable_fallback_fulltext", err
	}
	if query == "" {
		return nil, "", nil
	}

	g, gctx := errgroup.WithContext(ctx)
	var ftHits, semHits []memory.SearchHit
	var semErr error

	g.Go(func() error {
		var err error
		ftHits, err = h.Repo.SearchFulltext(gctx, query, tags, limit*2)
		return err
	})
	g.Go(func() error {
		vecs, err := h.Embedder.Embed(gctx, []string{query}, embeddings.InputQuery)
		if err != nil || len(vecs) == 0 {
			semErr = err
			return nil
		}
		semHits, err = h.Repo.SearchSemantic(gctx, vecs[0], tags, limit*2)
		return err
	})
	if err := g.Wait(); err != nil {
		return nil, "", err
	}

	if semErr != nil || semHits == nil {
		slog.Warn("hybrid: semantic leg failed; returning fulltext-only", "err", semErr)
		if len(ftHits) > limit {
			ftHits = ftHits[:limit]
		}
		return ftHits, "embedding_failed_fallback_fulltext", nil
	}

	fused := search.Fuse([][]memory.SearchHit{ftHits, semHits}, 60, limit)
	return fused, "", nil
}

func searchResp(mode memory.SearchMode, hits []memory.SearchHit, warning string) map[string]any {
	out := map[string]any{"mode": mode, "hits": hits}
	if warning != "" {
		out["warning"] = warning
	}
	return out
}

func invalidArg(msg string) *mcp.CallToolResult {
	return mcp.NewToolResultError("InvalidArgument: " + msg)
}

func mapRepoError(err error) (*mcp.CallToolResult, error) {
	switch {
	case errors.Is(err, memory.ErrMemoryNotFound):
		return mcp.NewToolResultError("MemoryNotFound: " + err.Error()), nil
	case errors.Is(err, memory.ErrTagNotFound):
		return mcp.NewToolResultError("TagNotFound: " + err.Error()), nil
	case errors.Is(err, memory.ErrInvalidArgs):
		return mcp.NewToolResultError("InvalidArgument: " + err.Error()), nil
	default:
		slog.Error("repository error", "err", err)
		return mcp.NewToolResultError("Internal: backend error"), nil
	}
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(b)), nil
}
