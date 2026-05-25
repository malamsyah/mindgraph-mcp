package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/dbtype"
)

type Repository struct {
	driver neo4j.DriverWithContext
}

func NewRepository(ctx context.Context, uri, user, password string) (*Repository, error) {
	driver, err := neo4j.NewDriverWithContext(uri, neo4j.BasicAuth(user, password, ""))
	if err != nil {
		return nil, fmt.Errorf("create neo4j driver: %w", err)
	}
	if err := driver.VerifyConnectivity(ctx); err != nil {
		_ = driver.Close(ctx)
		return nil, fmt.Errorf("verify neo4j connectivity: %w", err)
	}
	return &Repository{driver: driver}, nil
}

func (r *Repository) Close(ctx context.Context) error {
	return r.driver.Close(ctx)
}

// IsPermanentConnectError reports whether a NewRepository failure is one that
// retrying won't fix — bad credentials, security/cert issues. Network blips
// and other failures are treated as transient.
func IsPermanentConnectError(err error) bool {
	if err == nil {
		return false
	}
	var authErr *neo4j.InvalidAuthenticationError
	if errors.As(err, &authErr) {
		return true
	}
	var nErr *neo4j.Neo4jError
	if errors.As(err, &nErr) && strings.HasPrefix(nErr.Code, "Neo.ClientError.Security.") {
		return true
	}
	return false
}

// Bootstrap applies all constraints and indexes idempotently.
func (r *Repository) Bootstrap(ctx context.Context, embeddingDimensions int) error {
	statements := []string{
		`CREATE CONSTRAINT memory_id_unique IF NOT EXISTS
		   FOR (m:Memory) REQUIRE m.id IS UNIQUE`,
		`CREATE CONSTRAINT tag_name_unique IF NOT EXISTS
		   FOR (t:Tag) REQUIRE t.name IS UNIQUE`,
		`CREATE CONSTRAINT coderef_unique IF NOT EXISTS
		   FOR (c:CodeRef) REQUIRE (c.repo, c.path, c.sha, c.line) IS UNIQUE`,
		`CREATE FULLTEXT INDEX memory_content_fts IF NOT EXISTS
		   FOR (m:Memory) ON EACH [m.content]`,
		`CREATE INDEX memory_updated_at IF NOT EXISTS
		   FOR (m:Memory) ON (m.updated_at)`,
		`CREATE INDEX coderef_repo_path IF NOT EXISTS
		   FOR (c:CodeRef) ON (c.repo, c.path)`,
		fmt.Sprintf(`CREATE VECTOR INDEX memory_content_vec IF NOT EXISTS
		   FOR (m:Memory) ON (m.embedding)
		   OPTIONS { indexConfig: {
		     `+"`vector.dimensions`"+`: %d,
		     `+"`vector.similarity_function`"+`: 'cosine'
		   }}`, embeddingDimensions),
	}

	for _, stmt := range statements {
		if _, err := neo4j.ExecuteQuery(ctx, r.driver, stmt, nil,
			neo4j.EagerResultTransformer); err != nil {
			return fmt.Errorf("apply schema statement: %w\nstmt: %s", err, stmt)
		}
	}
	return nil
}

// AddMemory creates a Memory node, attaches tags, and returns the persisted
// shape. `embedding` may be nil for Phase 1 callers; Phase 2 fills it in.
func (r *Repository) AddMemory(ctx context.Context, content string, tags []string, embedding []float32) (*Memory, error) {
	if content == "" {
		return nil, fmt.Errorf("%w: content is required", ErrInvalidArgs)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generate uuidv7: %w", err)
	}

	normalized := NormalizeTags(tags)
	params := map[string]any{
		"id":      id.String(),
		"content": content,
		"tags":    toAnySlice(normalized),
	}
	if embedding != nil {
		params["embedding"] = float32SliceToAny(embedding)
	} else {
		params["embedding"] = nil
	}

	// FOREACH (not UNWIND) keeps a single row even when $tags is empty —
	// UNWIND over an empty list drops the row and the RETURN yields nothing.
	const cypher = `
		CREATE (m:Memory {
		  id: $id,
		  content: $content,
		  embedding: $embedding,
		  created_at: datetime(),
		  updated_at: datetime()
		})
		FOREACH (tag_name IN $tags |
		  MERGE (t:Tag {name: tag_name})
		  MERGE (m)-[:TAGGED_WITH]->(t)
		)
		RETURN m.id AS id, m.content AS content, m.created_at AS created_at, m.updated_at AS updated_at`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher, params, neo4j.EagerResultTransformer)
	if err != nil {
		return nil, fmt.Errorf("add memory: %w", err)
	}
	if len(res.Records) == 0 {
		return nil, errors.New("add memory: no rows returned")
	}
	return recordToMemory(res.Records[0])
}

// GetMemory returns a memory plus its tags, incoming/outgoing relationships,
// and any attached code references.
func (r *Repository) GetMemory(ctx context.Context, id string) (*MemoryDetail, error) {
	const cypher = `
		MATCH (m:Memory {id: $id})
		OPTIONAL MATCH (m)-[:TAGGED_WITH]->(t:Tag)
		OPTIONAL MATCH (m)-[out:RELATES_TO]->(target:Memory)
		OPTIONAL MATCH (source:Memory)-[in:RELATES_TO]->(m)
		OPTIONAL MATCH (m)-[:REFERENCES_CODE]->(c:CodeRef)
		RETURN
		  m.id AS id,
		  m.content AS content,
		  m.created_at AS created_at,
		  m.updated_at AS updated_at,
		  collect(DISTINCT t.name) AS tags,
		  collect(DISTINCT CASE WHEN target IS NULL THEN NULL ELSE {id: target.id, relationship: out.relationship} END) AS outgoing,
		  collect(DISTINCT CASE WHEN source IS NULL THEN NULL ELSE {id: source.id, relationship: in.relationship} END) AS incoming,
		  collect(DISTINCT CASE WHEN c IS NULL THEN NULL ELSE {repo: c.repo, path: c.path, sha: c.sha, line: c.line} END) AS code_refs`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher, map[string]any{"id": id}, neo4j.EagerResultTransformer)
	if err != nil {
		return nil, fmt.Errorf("get memory: %w", err)
	}
	if len(res.Records) == 0 {
		return nil, ErrMemoryNotFound
	}
	rec := res.Records[0]
	rawID, _, _ := neo4j.GetRecordValue[string](rec, "id")
	if rawID == "" {
		return nil, ErrMemoryNotFound
	}

	mem, err := recordToMemory(rec)
	if err != nil {
		return nil, err
	}

	detail := &MemoryDetail{Memory: *mem}
	if tags, ok := rec.Get("tags"); ok {
		for _, t := range tags.([]any) {
			if s, ok := t.(string); ok && s != "" {
				detail.Tags = append(detail.Tags, s)
			}
		}
	}
	if outs, ok := rec.Get("outgoing"); ok {
		detail.Outgoing = relationshipsFromAny(outs)
	}
	if ins, ok := rec.Get("incoming"); ok {
		detail.Incoming = relationshipsFromAny(ins)
	}
	if refs, ok := rec.Get("code_refs"); ok {
		detail.CodeRefs = codeRefsFromAny(refs)
	}
	return detail, nil
}

// AddCodeRef MERGEs a CodeRef node and a REFERENCES_CODE edge from the memory
// to it. Idempotent: calling twice with the same (memory_id, repo, path, sha,
// line) tuple yields a single edge.
//
// sha may be empty to mean "HEAD-relative — line numbers will drift". Pin sha
// for stable references. line may be 0 to mean a file-level reference.
func (r *Repository) AddCodeRef(ctx context.Context, memoryID, repo, path, sha string, line int) error {
	if memoryID == "" || repo == "" || path == "" {
		return fmt.Errorf("%w: memory_id, repo, and path are required", ErrInvalidArgs)
	}
	const cypher = `
		MATCH (m:Memory {id: $memory_id})
		MERGE (c:CodeRef {repo: $repo, path: $path, sha: $sha, line: $line})
		MERGE (m)-[:REFERENCES_CODE]->(c)
		RETURN m.id AS id`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher,
		map[string]any{
			"memory_id": memoryID,
			"repo":      repo,
			"path":      path,
			"sha":       sha,
			"line":      int64(line),
		}, neo4j.EagerResultTransformer)
	if err != nil {
		return fmt.Errorf("add code ref: %w", err)
	}
	if len(res.Records) == 0 {
		return ErrMemoryNotFound
	}
	return nil
}

// FindMemoriesByCode returns memories with at least one CodeRef pointing at
// the given (repo, path), regardless of sha or line. Sorted by updated_at DESC.
func (r *Repository) FindMemoriesByCode(ctx context.Context, repo, path string, limit int) ([]SearchHit, error) {
	if repo == "" || path == "" {
		return nil, fmt.Errorf("%w: repo and path are required", ErrInvalidArgs)
	}
	if limit <= 0 {
		limit = 20
	}
	const cypher = `
		MATCH (c:CodeRef {repo: $repo, path: $path})<-[:REFERENCES_CODE]-(m:Memory)
		RETURN DISTINCT m.id AS id, m.content AS content, m.updated_at AS updated_at
		ORDER BY m.updated_at DESC
		LIMIT $limit`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher,
		map[string]any{"repo": repo, "path": path, "limit": int64(limit)},
		neo4j.EagerResultTransformer)
	if err != nil {
		return nil, fmt.Errorf("find memories by code: %w", err)
	}
	hits := make([]SearchHit, 0, len(res.Records))
	for _, rec := range res.Records {
		hit, err := recordToHit(rec, false)
		if err != nil {
			return nil, err
		}
		hits = append(hits, hit)
	}
	return hits, nil
}

// DeleteMemory permanently removes a memory node along with all of its
// incoming and outgoing relationships. Tag nodes are left in place because
// they're shared and harmless when orphaned; future memories can reuse them.
//
// This is irreversible — there is no soft-delete in v1.
func (r *Repository) DeleteMemory(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidArgs)
	}
	const cypher = `
		MATCH (m:Memory {id: $id})
		WITH m, m.id AS deleted_id
		DETACH DELETE m
		RETURN deleted_id`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher,
		map[string]any{"id": id}, neo4j.EagerResultTransformer)
	if err != nil {
		return fmt.Errorf("delete memory: %w", err)
	}
	if len(res.Records) == 0 {
		return ErrMemoryNotFound
	}
	return nil
}

// DeleteTag removes a Tag node and all TAGGED_WITH edges pointing to it. The
// affected memories themselves survive — losing a tag never deletes a memory.
// Returns the number of memories that lost the tag.
func (r *Repository) DeleteTag(ctx context.Context, name string) (int, error) {
	name = NormalizeTagName(name)
	if name == "" {
		return 0, fmt.Errorf("%w: tag name is required", ErrInvalidArgs)
	}
	const cypher = `
		MATCH (t:Tag {name: $name})
		OPTIONAL MATCH (m:Memory)-[r:TAGGED_WITH]->(t)
		WITH t, count(r) AS affected
		DETACH DELETE t
		RETURN affected`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher,
		map[string]any{"name": name}, neo4j.EagerResultTransformer)
	if err != nil {
		return 0, fmt.Errorf("delete tag: %w", err)
	}
	if len(res.Records) == 0 {
		return 0, ErrTagNotFound
	}
	affected, _, _ := neo4j.GetRecordValue[int64](res.Records[0], "affected")
	return int(affected), nil
}

// RenameTag changes a tag's name. The new name must not already exist —
// callers wanting to fold one tag into another should use MergeTags instead.
// Returns the number of memories that were tagged with the renamed tag.
func (r *Repository) RenameTag(ctx context.Context, oldName, newName string) (int, error) {
	oldName = NormalizeTagName(oldName)
	newName = NormalizeTagName(newName)
	if oldName == "" || newName == "" {
		return 0, fmt.Errorf("%w: old_name and new_name are required", ErrInvalidArgs)
	}
	if oldName == newName {
		return 0, fmt.Errorf("%w: old_name and new_name are identical", ErrInvalidArgs)
	}

	// Pre-check to give a useful error rather than a constraint violation. Race
	// with concurrent writers isn't a concern in single-user v1.
	const existsQ = `MATCH (t:Tag {name: $name}) RETURN t.name AS name`
	exists, err := neo4j.ExecuteQuery(ctx, r.driver, existsQ,
		map[string]any{"name": newName}, neo4j.EagerResultTransformer)
	if err != nil {
		return 0, fmt.Errorf("rename tag exists-check: %w", err)
	}
	if len(exists.Records) > 0 {
		return 0, fmt.Errorf("%w: tag %q already exists; use merge_tags to combine", ErrInvalidArgs, newName)
	}

	const renameQ = `
		MATCH (t:Tag {name: $old})
		OPTIONAL MATCH (m:Memory)-[:TAGGED_WITH]->(t)
		WITH t, count(m) AS affected
		SET t.name = $new
		RETURN affected`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, renameQ,
		map[string]any{"old": oldName, "new": newName}, neo4j.EagerResultTransformer)
	if err != nil {
		return 0, fmt.Errorf("rename tag: %w", err)
	}
	if len(res.Records) == 0 {
		return 0, ErrTagNotFound
	}
	affected, _, _ := neo4j.GetRecordValue[int64](res.Records[0], "affected")
	return int(affected), nil
}

// MergeTags folds src into dst: every memory tagged with src ends up tagged
// with dst (idempotent — no duplicate edges), then src is deleted. Both tags
// must already exist; use RenameTag to relabel a tag that has no merge target.
// Returns the number of memories whose edges were rewritten.
func (r *Repository) MergeTags(ctx context.Context, src, dst string) (int, error) {
	src = NormalizeTagName(src)
	dst = NormalizeTagName(dst)
	if src == "" || dst == "" {
		return 0, fmt.Errorf("%w: source and target tag names are required", ErrInvalidArgs)
	}
	if src == dst {
		return 0, fmt.Errorf("%w: source and target must differ", ErrInvalidArgs)
	}

	const cypher = `
		MATCH (src:Tag {name: $src})
		MATCH (dst:Tag {name: $dst})
		OPTIONAL MATCH (m:Memory)-[:TAGGED_WITH]->(src)
		WITH src, dst, collect(DISTINCT m) AS memories
		WITH src, dst, memories, size(memories) AS affected
		FOREACH (mem IN memories | MERGE (mem)-[:TAGGED_WITH]->(dst))
		WITH src, affected
		DETACH DELETE src
		RETURN affected`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher,
		map[string]any{"src": src, "dst": dst}, neo4j.EagerResultTransformer)
	if err != nil {
		return 0, fmt.Errorf("merge tags: %w", err)
	}
	if len(res.Records) == 0 {
		return 0, ErrTagNotFound
	}
	affected, _, _ := neo4j.GetRecordValue[int64](res.Records[0], "affected")
	return int(affected), nil
}

// DeleteRelationship removes the directional RELATES_TO edge between from_id
// and to_id whose label matches relationship. Returns ErrRelationshipNotFound
// if no such edge exists (this also covers the case where either memory is
// missing — repos that want to distinguish should pre-check existence).
func (r *Repository) DeleteRelationship(ctx context.Context, fromID, toID, relationship string) error {
	if fromID == "" || toID == "" || relationship == "" {
		return fmt.Errorf("%w: from_id, to_id, and relationship are required", ErrInvalidArgs)
	}
	const cypher = `
		MATCH (from:Memory {id: $from_id})-[r:RELATES_TO {relationship: $relationship}]->(to:Memory {id: $to_id})
		WITH collect(r) AS rels, count(r) AS deleted
		FOREACH (rel IN rels | DELETE rel)
		RETURN deleted`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher,
		map[string]any{
			"from_id":      fromID,
			"to_id":        toID,
			"relationship": relationship,
		}, neo4j.EagerResultTransformer)
	if err != nil {
		return fmt.Errorf("delete relationship: %w", err)
	}
	if len(res.Records) == 0 {
		return ErrRelationshipNotFound
	}
	deleted, _, _ := neo4j.GetRecordValue[int64](res.Records[0], "deleted")
	if deleted == 0 {
		return ErrRelationshipNotFound
	}
	return nil
}

// LinkMemories MERGEs a RELATES_TO edge with the given relationship label.
// Parallel edges with different relationship strings are allowed (see SPEC §14).
func (r *Repository) LinkMemories(ctx context.Context, fromID, toID, relationship string) error {
	if fromID == "" || toID == "" || relationship == "" {
		return fmt.Errorf("%w: from_id, to_id, and relationship are required", ErrInvalidArgs)
	}
	const cypher = `
		MATCH (from:Memory {id: $from_id})
		MATCH (to:Memory {id: $to_id})
		MERGE (from)-[r:RELATES_TO {relationship: $relationship}]->(to)
		RETURN from.id AS from_id, to.id AS to_id, r.relationship AS relationship`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher,
		map[string]any{"from_id": fromID, "to_id": toID, "relationship": relationship},
		neo4j.EagerResultTransformer)
	if err != nil {
		return fmt.Errorf("link memories: %w", err)
	}
	if len(res.Records) == 0 {
		return ErrMemoryNotFound
	}
	return nil
}

// ListRecent returns memories ordered by updated_at DESC, optional tag filter.
func (r *Repository) ListRecent(ctx context.Context, tags []string, limit int) ([]SearchHit, error) {
	if limit <= 0 {
		limit = 10
	}
	normalized := NormalizeTags(tags)
	const cypher = `
		MATCH (m:Memory)
		WHERE size($tags) = 0 OR ALL(tag IN $tags WHERE
		  EXISTS { MATCH (m)-[:TAGGED_WITH]->(:Tag {name: tag}) }
		)
		RETURN m.id AS id, m.content AS content, m.updated_at AS updated_at
		ORDER BY m.updated_at DESC
		LIMIT $limit`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher,
		map[string]any{"tags": toAnySlice(normalized), "limit": int64(limit)},
		neo4j.EagerResultTransformer)
	if err != nil {
		return nil, fmt.Errorf("list recent: %w", err)
	}
	hits := make([]SearchHit, 0, len(res.Records))
	for _, rec := range res.Records {
		hit, err := recordToHit(rec, false)
		if err != nil {
			return nil, err
		}
		hits = append(hits, hit)
	}
	return hits, nil
}

// SearchFulltext runs a Lucene-style fulltext query against memory content
// with optional tag filter.
func (r *Repository) SearchFulltext(ctx context.Context, query string, tags []string, limit int) ([]SearchHit, error) {
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	normalized := NormalizeTags(tags)
	const cypher = `
		CALL db.index.fulltext.queryNodes("memory_content_fts", $query) YIELD node, score
		WITH node, score
		WHERE size($tags) = 0 OR ALL(tag IN $tags WHERE
		  EXISTS { MATCH (node)-[:TAGGED_WITH]->(:Tag {name: tag}) }
		)
		RETURN node.id AS id, node.content AS content, node.updated_at AS updated_at, score
		ORDER BY score DESC
		LIMIT $limit`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher,
		map[string]any{"query": query, "tags": toAnySlice(normalized), "limit": int64(limit)},
		neo4j.EagerResultTransformer)
	if err != nil {
		return nil, fmt.Errorf("search fulltext: %w", err)
	}
	hits := make([]SearchHit, 0, len(res.Records))
	for _, rec := range res.Records {
		hit, err := recordToHit(rec, true)
		if err != nil {
			return nil, err
		}
		hits = append(hits, hit)
	}
	return hits, nil
}

// SearchSemantic runs a vector kNN search against the memory_content_vec index
// and applies an optional tag filter post-ranking. A candidate multiplier
// (limit*3) gives the tag filter room to drop matches without starving the
// result set.
func (r *Repository) SearchSemantic(ctx context.Context, queryVec []float32, tags []string, limit int) ([]SearchHit, error) {
	if len(queryVec) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	normalized := NormalizeTags(tags)
	candidateLimit := limit * 3
	const cypher = `
		CALL db.index.vector.queryNodes("memory_content_vec", $candidate_limit, $query_vector)
		YIELD node, score
		WITH node, score
		WHERE size($tags) = 0 OR ALL(tag IN $tags WHERE
		  EXISTS { MATCH (node)-[:TAGGED_WITH]->(:Tag {name: tag}) }
		)
		RETURN node.id AS id, node.content AS content, node.updated_at AS updated_at, score
		ORDER BY score DESC
		LIMIT $limit`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher,
		map[string]any{
			"candidate_limit": int64(candidateLimit),
			"query_vector":    float32SliceToAny(queryVec),
			"tags":            toAnySlice(normalized),
			"limit":           int64(limit),
		},
		neo4j.EagerResultTransformer)
	if err != nil {
		return nil, fmt.Errorf("search semantic: %w", err)
	}
	hits := make([]SearchHit, 0, len(res.Records))
	for _, rec := range res.Records {
		hit, err := recordToHit(rec, true)
		if err != nil {
			return nil, err
		}
		hits = append(hits, hit)
	}
	return hits, nil
}

// MissingEmbeddings returns IDs and content for memories with NULL embedding,
// for one-shot Phase 2 backfill.
func (r *Repository) MissingEmbeddings(ctx context.Context, limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 128
	}
	const cypher = `
		MATCH (m:Memory)
		WHERE m.embedding IS NULL
		RETURN m.id AS id, m.content AS content, m.created_at AS created_at, m.updated_at AS updated_at
		LIMIT $limit`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher,
		map[string]any{"limit": int64(limit)}, neo4j.EagerResultTransformer)
	if err != nil {
		return nil, fmt.Errorf("missing embeddings: %w", err)
	}
	out := make([]Memory, 0, len(res.Records))
	for _, rec := range res.Records {
		mem, err := recordToMemory(rec)
		if err != nil {
			return nil, err
		}
		out = append(out, *mem)
	}
	return out, nil
}

// ListTags returns every Tag in the graph with the number of memories tagged
// with it, sorted by count DESC, then name ASC for stable ordering of ties.
// Unbounded result set — personal graphs are expected to stay under ~hundreds
// of tags, so pagination isn't worth the complexity.
func (r *Repository) ListTags(ctx context.Context) ([]TagCount, error) {
	const cypher = `
		MATCH (t:Tag)
		OPTIONAL MATCH (m:Memory)-[:TAGGED_WITH]->(t)
		WITH t.name AS name, count(m) AS count
		RETURN name, count
		ORDER BY count DESC, name ASC`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher, nil, neo4j.EagerResultTransformer)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	out := make([]TagCount, 0, len(res.Records))
	for _, rec := range res.Records {
		name, _, _ := neo4j.GetRecordValue[string](rec, "name")
		count, _, _ := neo4j.GetRecordValue[int64](rec, "count")
		out = append(out, TagCount{Name: name, Count: int(count)})
	}
	return out, nil
}

// ListMemoriesPaged returns a single page of memories ordered by UUIDv7 id
// ASC (i.e. creation order). Content is truncated to previewLen to keep the
// payload bounded; full content is via get_memory. NextAfterID is set when
// another page may exist.
func (r *Repository) ListMemoriesPaged(ctx context.Context, afterID string, limit, previewLen int) (*MemoriesPage, error) {
	if limit <= 0 {
		limit = 50
	}
	if previewLen <= 0 {
		previewLen = 200
	}
	cypher := fmt.Sprintf(`
		MATCH (m:Memory)
		WHERE $after_id = "" OR m.id > $after_id
		RETURN m.id AS id, substring(m.content, 0, %d) AS content_preview, m.updated_at AS updated_at
		ORDER BY m.id ASC
		LIMIT $limit`, previewLen)
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher,
		map[string]any{"after_id": afterID, "limit": int64(limit)},
		neo4j.EagerResultTransformer)
	if err != nil {
		return nil, fmt.Errorf("list memories: %w", err)
	}
	page := &MemoriesPage{Items: make([]MemoryPreview, 0, len(res.Records))}
	for _, rec := range res.Records {
		id, _, _ := neo4j.GetRecordValue[string](rec, "id")
		preview, _, _ := neo4j.GetRecordValue[string](rec, "content_preview")
		updatedAt, err := recordTime(rec, "updated_at")
		if err != nil {
			return nil, fmt.Errorf("decode updated_at: %w", err)
		}
		page.Items = append(page.Items, MemoryPreview{ID: id, ContentPreview: preview, UpdatedAt: updatedAt})
	}
	if len(page.Items) == limit {
		page.NextAfterID = page.Items[len(page.Items)-1].ID
	}
	return page, nil
}

// ListRelationships returns every distinct RELATES_TO label with usage count,
// sorted by count DESC, then label ASC.
func (r *Repository) ListRelationships(ctx context.Context) ([]RelationshipCount, error) {
	const cypher = `
		MATCH ()-[r:RELATES_TO]->()
		WITH r.relationship AS label, count(r) AS count
		RETURN label, count
		ORDER BY count DESC, label ASC`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher, nil, neo4j.EagerResultTransformer)
	if err != nil {
		return nil, fmt.Errorf("list relationships: %w", err)
	}
	out := make([]RelationshipCount, 0, len(res.Records))
	for _, rec := range res.Records {
		label, _, _ := neo4j.GetRecordValue[string](rec, "label")
		count, _, _ := neo4j.GetRecordValue[int64](rec, "count")
		out = append(out, RelationshipCount{Label: label, Count: int(count)})
	}
	return out, nil
}

// ListAllForReembed returns memories with id > afterID, ordered by id ASC, up
// to limit. Used for full re-embed (e.g. recovering from a batch of failed
// embedding calls) where target memories may already have a stored — but
// incorrect — embedding, so we can't rely on the NULL filter. UUIDv7 IDs are
// time-ordered, so id-cursor pagination is stable and creation-order.
func (r *Repository) ListAllForReembed(ctx context.Context, afterID string, limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 128
	}
	const cypher = `
		MATCH (m:Memory)
		WHERE $after_id = "" OR m.id > $after_id
		RETURN m.id AS id, m.content AS content, m.created_at AS created_at, m.updated_at AS updated_at
		ORDER BY m.id ASC
		LIMIT $limit`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher,
		map[string]any{"after_id": afterID, "limit": int64(limit)},
		neo4j.EagerResultTransformer)
	if err != nil {
		return nil, fmt.Errorf("list all for reembed: %w", err)
	}
	out := make([]Memory, 0, len(res.Records))
	for _, rec := range res.Records {
		mem, err := recordToMemory(rec)
		if err != nil {
			return nil, err
		}
		out = append(out, *mem)
	}
	return out, nil
}

// GetMemoryContent returns just the content of a memory by id, used by the
// single-id reembed path. Returns ErrMemoryNotFound if no such memory.
func (r *Repository) GetMemoryContent(ctx context.Context, id string) (string, error) {
	const cypher = `MATCH (m:Memory {id: $id}) RETURN m.content AS content`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher,
		map[string]any{"id": id}, neo4j.EagerResultTransformer)
	if err != nil {
		return "", fmt.Errorf("get memory content: %w", err)
	}
	if len(res.Records) == 0 {
		return "", ErrMemoryNotFound
	}
	content, _, _ := neo4j.GetRecordValue[string](res.Records[0], "content")
	return content, nil
}

// UpdateEmbedding writes an embedding vector to an existing memory.
func (r *Repository) UpdateEmbedding(ctx context.Context, id string, vec []float32) error {
	const cypher = `
		MATCH (m:Memory {id: $id})
		SET m.embedding = $embedding
		RETURN m.id AS id`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher,
		map[string]any{"id": id, "embedding": float32SliceToAny(vec)},
		neo4j.EagerResultTransformer)
	if err != nil {
		return fmt.Errorf("update embedding: %w", err)
	}
	if len(res.Records) == 0 {
		return ErrMemoryNotFound
	}
	return nil
}

// FindPath returns the shortest undirected RELATES_TO path between two memories,
// up to maxHops in length. Returns PathResult with Hops=-1 if no path exists.
// maxHops MUST be a validated int in [1, 6] — it is interpolated into the
// query because Cypher cannot parameterize variable-length path bounds.
func (r *Repository) FindPath(ctx context.Context, fromID, toID string, maxHops int) (*PathResult, error) {
	if fromID == "" || toID == "" {
		return nil, fmt.Errorf("%w: from_id and to_id are required", ErrInvalidArgs)
	}
	if maxHops < 1 || maxHops > 6 {
		return nil, fmt.Errorf("%w: max_hops must be in [1, 6]", ErrInvalidArgs)
	}
	cypher := fmt.Sprintf(`
		MATCH (from:Memory {id: $from_id})
		MATCH (to:Memory {id: $to_id})
		MATCH path = shortestPath((from)-[:RELATES_TO*1..%d]-(to))
		RETURN
		  [n IN nodes(path) | {id: n.id, content: n.content}] AS nodes,
		  [r IN relationships(path) | r.relationship] AS relationships,
		  length(path) AS hops`, maxHops)
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher,
		map[string]any{"from_id": fromID, "to_id": toID}, neo4j.EagerResultTransformer)
	if err != nil {
		return nil, fmt.Errorf("find path: %w", err)
	}
	if len(res.Records) == 0 {
		// Verify both nodes exist; otherwise return MemoryNotFound.
		if missing, mErr := r.endpointsExist(ctx, fromID, toID); mErr == nil && missing {
			return nil, ErrMemoryNotFound
		}
		return &PathResult{Nodes: nil, Relationships: nil, Hops: -1}, nil
	}
	rec := res.Records[0]

	nodesRaw, _ := rec.Get("nodes")
	relsRaw, _ := rec.Get("relationships")
	hopsRaw, _, _ := neo4j.GetRecordValue[int64](rec, "hops")

	pr := &PathResult{Hops: int(hopsRaw)}
	if nodesAny, ok := nodesRaw.([]any); ok {
		for _, n := range nodesAny {
			m, _ := n.(map[string]any)
			pr.Nodes = append(pr.Nodes, PathNode{
				ID:      stringOr(m["id"]),
				Content: stringOr(m["content"]),
			})
		}
	}
	if relsAny, ok := relsRaw.([]any); ok {
		for _, r := range relsAny {
			pr.Relationships = append(pr.Relationships, stringOr(r))
		}
	}
	return pr, nil
}

// FindRelated returns memories within hops of the start node, sorted by
// graph distance ascending. hops MUST be validated to [1, 4]; relationship
// filter (when non-empty) restricts which RELATES_TO labels count.
func (r *Repository) FindRelated(ctx context.Context, id string, hops int, relFilter []string, limit int) ([]RelatedMemory, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: id is required", ErrInvalidArgs)
	}
	if hops < 1 || hops > 4 {
		return nil, fmt.Errorf("%w: hops must be in [1, 4]", ErrInvalidArgs)
	}
	if limit <= 0 {
		limit = 20
	}
	cypher := fmt.Sprintf(`
		MATCH (start:Memory {id: $id})
		MATCH (start)-[r:RELATES_TO*1..%d]-(related:Memory)
		WHERE related <> start
		  AND (size($rel_filter) = 0 OR ALL(rel IN r WHERE rel.relationship IN $rel_filter))
		WITH related, min(size(r)) AS distance
		RETURN related.id AS id, related.content AS content, distance
		ORDER BY distance ASC, related.updated_at DESC
		LIMIT $limit`, hops)
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher,
		map[string]any{
			"id":         id,
			"rel_filter": toAnySlice(relFilter),
			"limit":      int64(limit),
		},
		neo4j.EagerResultTransformer)
	if err != nil {
		return nil, fmt.Errorf("find related: %w", err)
	}
	// Empty result could mean orphan or non-existent start; disambiguate.
	if len(res.Records) == 0 {
		if missing, mErr := r.endpointsExist(ctx, id, id); mErr == nil && missing {
			return nil, ErrMemoryNotFound
		}
		return nil, nil
	}
	out := make([]RelatedMemory, 0, len(res.Records))
	for _, rec := range res.Records {
		idStr, _, _ := neo4j.GetRecordValue[string](rec, "id")
		content, _, _ := neo4j.GetRecordValue[string](rec, "content")
		dist, _, _ := neo4j.GetRecordValue[int64](rec, "distance")
		out = append(out, RelatedMemory{ID: idStr, Content: content, Distance: int(dist)})
	}
	return out, nil
}

// endpointsExist returns (anyMissing, error). If either node is missing, the
// first return is true.
func (r *Repository) endpointsExist(ctx context.Context, a, b string) (bool, error) {
	const cypher = `
		OPTIONAL MATCH (x:Memory {id: $a})
		OPTIONAL MATCH (y:Memory {id: $b})
		RETURN x IS NULL OR y IS NULL AS missing`
	res, err := neo4j.ExecuteQuery(ctx, r.driver, cypher,
		map[string]any{"a": a, "b": b}, neo4j.EagerResultTransformer)
	if err != nil {
		return false, err
	}
	if len(res.Records) == 0 {
		return true, nil
	}
	missing, _, _ := neo4j.GetRecordValue[bool](res.Records[0], "missing")
	return missing, nil
}

// recordToMemory extracts the id/content/created_at/updated_at fields from a
// row. Datetime properties come back as dbtype.Time / time.Time depending on
// the Cypher cast; we normalize via the driver's RecordValue interface.
func recordToMemory(rec *neo4j.Record) (*Memory, error) {
	id, _, _ := neo4j.GetRecordValue[string](rec, "id")
	content, _, _ := neo4j.GetRecordValue[string](rec, "content")
	createdAt, err := recordTime(rec, "created_at")
	if err != nil {
		return nil, fmt.Errorf("decode created_at: %w", err)
	}
	updatedAt, err := recordTime(rec, "updated_at")
	if err != nil {
		return nil, fmt.Errorf("decode updated_at: %w", err)
	}
	return &Memory{ID: id, Content: content, CreatedAt: createdAt, UpdatedAt: updatedAt}, nil
}

func recordToHit(rec *neo4j.Record, hasScore bool) (SearchHit, error) {
	id, _, _ := neo4j.GetRecordValue[string](rec, "id")
	content, _, _ := neo4j.GetRecordValue[string](rec, "content")
	updatedAt, err := recordTime(rec, "updated_at")
	if err != nil {
		return SearchHit{}, fmt.Errorf("decode updated_at: %w", err)
	}
	hit := SearchHit{ID: id, Content: content, UpdatedAt: updatedAt}
	if hasScore {
		if score, _, _ := neo4j.GetRecordValue[float64](rec, "score"); score != 0 {
			hit.Score = score
		}
	}
	return hit, nil
}

// recordTime accepts either time.Time (when datetime returned directly) or
// dbtype.Time (Cypher temporal types). Memory timestamps are always datetime,
// so dbtype.Time is the relevant case for some driver paths.
func recordTime(rec *neo4j.Record, key string) (time.Time, error) {
	v, ok := rec.Get(key)
	if !ok || v == nil {
		return time.Time{}, nil
	}
	switch t := v.(type) {
	case time.Time:
		return t, nil
	case dbtype.Time:
		return t.Time(), nil
	case dbtype.LocalDateTime:
		return t.Time(), nil
	case dbtype.Date:
		return t.Time(), nil
	}
	return time.Time{}, fmt.Errorf("unexpected datetime type %T for key %s", v, key)
}

func relationshipsFromAny(v any) []Relationship {
	out := []Relationship{}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id := stringOr(m["id"])
		rel := stringOr(m["relationship"])
		if id == "" {
			continue
		}
		out = append(out, Relationship{ID: id, Relationship: rel})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func codeRefsFromAny(v any) []CodeRef {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]CodeRef, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		repo := stringOr(m["repo"])
		if repo == "" {
			continue // null/empty entries from OPTIONAL MATCH
		}
		ref := CodeRef{
			Repo: repo,
			Path: stringOr(m["path"]),
			Sha:  stringOr(m["sha"]),
		}
		if l, ok := m["line"].(int64); ok {
			ref.Line = int(l)
		}
		out = append(out, ref)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func stringOr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func float32SliceToAny(in []float32) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}
