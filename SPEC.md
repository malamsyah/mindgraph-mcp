# SPEC — MCP Graph Memory Server

Project name: `mindgraph` · Repo: `mindgraph-mcp` · Go module: `github.com/malamsyah/mindgraph-mcp`.

A remote MCP server that exposes a graph-backed memory store to Claude clients (Claude.ai, Claude Code) so that context, decisions, and notes persist across sessions, can be searched semantically and lexically, and can be queried and traversed as a personal knowledge graph.

Structured in three phases. Each phase has a clear exit criterion; advance only when met.

---

## 1. Goals & non-goals

### Goals (v1)

- Single-user, personal cross-session memory store accessible from any Claude client via MCP.
- Graph-shaped storage; relationships between memories are first-class.
- **Semantic search via best-in-class vector embeddings**, with full-text and tag filters available as alternative search modes.
- **Graph traversal queries**: shortest path between two memories; n-hop neighborhood discovery.
- Deployed with no cold-start latency for daily-use feel.
- Solid enough to use daily.

### Non-goals (v1, explicitly)

- Multi-user / multi-tenancy. Single API key.
- Web UI. Claude is the client.
- Streaming, file uploads, binary content. Memories are markdown strings.
- Real-time sync / conflict resolution. Last-write-wins.
- Memory versioning / history.
- Bulk import / export.
- Tag rename / merge.
- Memory delete (append-only in v1 — see open questions).

---

## 2. Architecture

```
                 ┌──────────────────┐
   Claude.ai ───▶│                  │
                 │   mindgraph      │       ┌──────────────────────┐
   Claude Code ─▶│  (Cloud Run,     │──────▶│  Neo4j AuraDB        │
                 │   min-instances  │ bolt+s│  Professional         │
   Other MCP ───▶│   = 1, no cold   │       └──────────────────────┘
   clients       │   start)         │
                 │   Go + mcp-go    │       ┌──────────────────────┐
                 │  Streamable HTTP │──────▶│   Voyage AI          │
                 └──────────────────┘ https │   (voyage-3-large)   │
                         ▲                  └──────────────────────┘
                         │ API key
                         │ (Authorization: Bearer)
                         │
                 ┌───────┴────────┐
                 │  Secret Manager │
                 │  (GCP)          │
                 └────────────────┘
```

### Component choices (best-in-class)

| Component | Choice | Rationale |
|---|---|---|
| Language | Go 1.22+ | User's stack; aligns with `mark3labs/mcp-go` upstream contribution goal. |
| MCP SDK | `github.com/mark3labs/mcp-go` | Most mature Go MCP SDK. |
| Transport | Streamable HTTP (HTTP + SSE) | Current MCP standard for remote servers. |
| Graph DB | **Neo4j AuraDB Professional** | Best managed Neo4j; no row/edge caps; high-availability; native vector index; Cypher. |
| Embeddings | **Voyage AI `voyage-3-large`** | Anthropic-recommended; flagship model; 2048-dim output for highest retrieval quality. |
| Deploy | **Cloud Run, min instances = 1** | No cold-start latency; auto-HTTPS; container-based; managed. |
| Secrets | **Google Secret Manager** | Avoids plaintext env vars; rotatable; auditable. |
| Auth (app-level) | API key in `Authorization: Bearer <key>` | Single user. |
| ID generation | UUIDv7 via `github.com/google/uuid` | Time-ordered, sortable. |

> **Verify before implementation:** Voyage AI's current flagship model name and default dimensions. As of spec writing, `voyage-3-large` supports configurable output dimensions (256 / 512 / 1024 / 2048). Spec assumes 2048 for maximum retrieval quality.

---

## 3. Data model

### Nodes

**`Memory`**
| Property | Type | Notes |
|---|---|---|
| `id` | string (UUIDv7) | Primary identifier. |
| `content` | string (markdown) | Memory body. Practical limit ~100KB. |
| `embedding` | list<float> (2048 dims) | Voyage AI `voyage-3-large` embedding. Set on insert and on content update. |
| `created_at` | datetime | Set on insert; immutable. |
| `updated_at` | datetime | Set on insert; updated on content change. |

**`Tag`**
| Property | Type | Notes |
|---|---|---|
| `name` | string | Unique, lowercase, no whitespace. |

### Edges

- **`(:Memory)-[:TAGGED_WITH]->(:Tag)`** — tagging.
- **`(:Memory)-[:RELATES_TO {relationship: string}]->(:Memory)`** — typed relationships. Free-form string (e.g., `"refines"`, `"contradicts"`, `"follows-up"`, `"context-for"`).

### Indexes / constraints (applied idempotently on boot)

```cypher
CREATE CONSTRAINT memory_id_unique IF NOT EXISTS
  FOR (m:Memory) REQUIRE m.id IS UNIQUE;

CREATE CONSTRAINT tag_name_unique IF NOT EXISTS
  FOR (t:Tag) REQUIRE t.name IS UNIQUE;

CREATE FULLTEXT INDEX memory_content_fts IF NOT EXISTS
  FOR (m:Memory) ON EACH [m.content];

CREATE INDEX memory_updated_at IF NOT EXISTS
  FOR (m:Memory) ON (m.updated_at);

CREATE VECTOR INDEX memory_content_vec IF NOT EXISTS
  FOR (m:Memory) ON (m.embedding)
  OPTIONS { indexConfig: {
    `vector.dimensions`: 2048,
    `vector.similarity_function`: 'cosine'
  }};
```

---

## 4. MCP tool surface

Seven tools.

| Tool | Purpose | Phase |
|---|---|---|
| `add_memory` | Create memory + tags | 1 |
| `search_memory` | Search by full-text, semantic, or hybrid mode | 1 (fulltext) + 2 (semantic, hybrid) |
| `get_memory` | Fetch one with tags + relationships | 1 |
| `link_memories` | Create / update relationship between two memories | 1 |
| `list_recent` | Most recently updated | 1 |
| `find_path` | Shortest path between two memories | 2 |
| `find_related` | N-hop neighborhood of a memory | 2 |

### 4.1 `add_memory`

```json
{
  "type": "object",
  "properties": {
    "content": { "type": "string", "minLength": 1 },
    "tags":    { "type": "array", "items": { "type": "string" }, "default": [] }
  },
  "required": ["content"]
}
```

Behavior: generate UUIDv7; normalize tags (lowercase, trim, drop empties); compute Voyage AI embedding (Phase 2; null in Phase 1); persist node + tags.

```cypher
CREATE (m:Memory {
  id: $id,
  content: $content,
  embedding: $embedding,
  created_at: datetime(),
  updated_at: datetime()
})
WITH m
UNWIND $tags AS tag_name
MERGE (t:Tag {name: tag_name})
MERGE (m)-[:TAGGED_WITH]->(t)
RETURN m.id AS id, m.content AS content, m.created_at AS created_at;
```

### 4.2 `search_memory`

One tool, three modes. Default `hybrid`.

```json
{
  "type": "object",
  "properties": {
    "query": { "type": "string" },
    "mode":  { "type": "string", "enum": ["fulltext", "semantic", "hybrid"], "default": "hybrid" },
    "tags":  { "type": "array", "items": { "type": "string" }, "default": [] },
    "limit": { "type": "integer", "minimum": 1, "maximum": 50, "default": 10 }
  }
}
```

**Fulltext mode**
```cypher
CALL db.index.fulltext.queryNodes("memory_content_fts", $query) YIELD node, score
WHERE size($tags) = 0 OR ALL(tag IN $tags WHERE
  EXISTS { MATCH (node)-[:TAGGED_WITH]->(:Tag {name: tag}) }
)
RETURN node.id AS id, node.content AS content, node.updated_at AS updated_at, score
ORDER BY score DESC
LIMIT $limit;
```

**Semantic mode** (embed query first with Voyage AI, input type `query`)
```cypher
CALL db.index.vector.queryNodes("memory_content_vec", $candidate_limit, $query_vector)
YIELD node, score
WHERE size($tags) = 0 OR ALL(tag IN $tags WHERE
  EXISTS { MATCH (node)-[:TAGGED_WITH]->(:Tag {name: tag}) }
)
RETURN node.id AS id, node.content AS content, node.updated_at AS updated_at, score
LIMIT $limit;
```

`$candidate_limit = limit * 3` to give the tag filter room to drop matches.

**Hybrid mode (application-side fusion)**
1. Run fulltext + semantic queries in parallel, each requesting `limit * 2`.
2. Rank each list 1..N.
3. RRF: `score = sum(1 / (k + rank))` across both lists, `k = 60`.
4. Sort by fused score; return top `limit`.

### 4.3 `get_memory`

```cypher
MATCH (m:Memory {id: $id})
OPTIONAL MATCH (m)-[:TAGGED_WITH]->(t:Tag)
OPTIONAL MATCH (m)-[out:RELATES_TO]->(target:Memory)
OPTIONAL MATCH (source:Memory)-[in:RELATES_TO]->(m)
RETURN
  m,
  collect(DISTINCT t.name) AS tags,
  collect(DISTINCT {id: target.id, relationship: out.relationship}) AS outgoing,
  collect(DISTINCT {id: source.id, relationship: in.relationship}) AS incoming;
```

### 4.4 `link_memories`

```cypher
MATCH (from:Memory {id: $from_id}), (to:Memory {id: $to_id})
MERGE (from)-[r:RELATES_TO {relationship: $relationship}]->(to)
RETURN from.id AS from_id, to.id AS to_id, r.relationship AS relationship;
```

### 4.5 `list_recent`

```cypher
MATCH (m:Memory)
WHERE size($tags) = 0 OR ALL(tag IN $tags WHERE
  EXISTS { MATCH (m)-[:TAGGED_WITH]->(:Tag {name: tag}) }
)
RETURN m.id AS id, m.content AS content, m.updated_at AS updated_at
ORDER BY m.updated_at DESC
LIMIT $limit;
```

### 4.6 `find_path`

Shortest path between two memories along `RELATES_TO` edges (undirected by default — see open questions).

```json
{
  "type": "object",
  "properties": {
    "from_id":  { "type": "string" },
    "to_id":    { "type": "string" },
    "max_hops": { "type": "integer", "minimum": 1, "maximum": 6, "default": 4 }
  },
  "required": ["from_id", "to_id"]
}
```

```cypher
// max_hops interpolated as int after validation (Cypher can't parameterize path bounds)
MATCH (from:Memory {id: $from_id}), (to:Memory {id: $to_id})
MATCH path = shortestPath((from)-[:RELATES_TO*1..MAX_HOPS]-(to))
RETURN
  [n IN nodes(path) | {id: n.id, content: n.content}] AS nodes,
  [r IN relationships(path) | r.relationship]         AS relationships,
  length(path)                                        AS hops;
```

Returns null path with `hops = -1` if no path within `max_hops`.

### 4.7 `find_related`

Memories within N hops of a starting memory.

```json
{
  "type": "object",
  "properties": {
    "id":                  { "type": "string" },
    "hops":                { "type": "integer", "minimum": 1, "maximum": 4, "default": 2 },
    "relationship_filter": { "type": "array", "items": { "type": "string" }, "default": [] },
    "limit":               { "type": "integer", "minimum": 1, "maximum": 50, "default": 20 }
  },
  "required": ["id"]
}
```

```cypher
// hops interpolated as int after validation
MATCH (start:Memory {id: $id})
MATCH (start)-[r:RELATES_TO*1..HOPS]-(related:Memory)
WHERE related <> start
  AND (size($rel_filter) = 0 OR ALL(rel IN r WHERE rel.relationship IN $rel_filter))
WITH related, min(length(r)) AS distance
RETURN related.id AS id,
       related.content AS content,
       distance
ORDER BY distance ASC
LIMIT $limit;
```

---

## 5. Embeddings

### Voyage AI integration

- Endpoint: `https://api.voyageai.com/v1/embeddings`
- Model: **`voyage-3-large`** with `output_dimension = 2048`
- Auth: `Authorization: Bearer $VOYAGE_API_KEY`
- Input type: `document` for memory content; `query` for search.

### Lifecycle

| Event | Action |
|---|---|
| `add_memory` | Generate embedding (input type `document`), store in `Memory.embedding`. |
| Content update (v1.1) | Re-embed and update. |
| `search_memory` semantic/hybrid | Embed query (input type `query`); vector search. |
| Server boot, embedding missing on existing memories | Backfill: query `embedding IS NULL`, embed in batches of 128, update. Run once at Phase 2 deploy. |

### Failure modes

- Voyage AI transient error on `add_memory` → retry with exponential backoff (3 attempts); if still failing, persist memory with `embedding = null` and surface a warning in the response. A separate sweep can re-embed nulls later.
- Voyage AI outage on `search_memory` semantic/hybrid → fall back to fulltext with a warning in response.

---

## 6. Authentication & configuration

API key in `Authorization: Bearer <key>`. Constant-time compare. 401 on missing/invalid.

Secrets pulled from Google Secret Manager at boot (with env override for local dev).

| Config | Source |
|---|---|
| `MINDGRAPH_API_KEY` | Secret Manager `mindgraph-api-key` |
| `NEO4J_URI` | Secret Manager `neo4j-uri` (e.g., `neo4j+s://...`) |
| `NEO4J_USER` | Secret Manager `neo4j-user` |
| `NEO4J_PASSWORD` | Secret Manager `neo4j-password` |
| `VOYAGE_API_KEY` | Secret Manager `voyage-api-key` |
| `EMBEDDING_MODEL` | Env var, default `voyage-3-large` |
| `EMBEDDING_DIMENSIONS` | Env var, default `2048` |
| `PORT` | `8080` (Cloud Run sets) |
| `LOG_LEVEL` | Env var, default `info` |

---

## 7. Deployment

### Cloud Run

| Setting | Value |
|---|---|
| CPU | 2 |
| Memory | 1 GiB (room for 2048-dim vector ops + embedding payloads) |
| **Min instances** | **1** (no cold start) |
| Max instances | 4 |
| Concurrency | 80 |
| Timeout | 60s |
| Ingress | All |
| Auth | Allow unauthenticated (app-level API key gate) |
| Region | `asia-northeast1` (Tokyo) — match user latency |

Multi-stage Dockerfile → `gcr.io/distroless/static:nonroot` → Artifact Registry → Cloud Run.

### Neo4j AuraDB Professional

- Single instance, region `asia-northeast1` (Tokyo) to match Cloud Run.
- Default instance size (sufficient for personal use; scale up later if needed).
- Automated backups enabled.

---

## 8. Project structure

```
mindgraph-mcp/
├── cmd/server/main.go              # entry, wiring
├── internal/
│   ├── config/config.go            # env + Secret Manager
│   ├── memory/
│   │   ├── model.go                # Memory, Tag, Relationship types
│   │   ├── repository.go           # Neo4j CRUD + search + traversal
│   │   └── repository_test.go
│   ├── embeddings/
│   │   ├── client.go               # Voyage AI client
│   │   ├── embedder.go             # interface (swappable)
│   │   └── client_test.go
│   ├── search/
│   │   ├── fusion.go               # RRF score combination
│   │   └── fusion_test.go
│   ├── mcp/
│   │   ├── server.go               # mcp-go setup
│   │   └── tools.go                # 7 tool handlers
│   └── auth/middleware.go
├── Dockerfile
├── go.mod
├── go.sum
├── README.md
├── SPEC.md                          # this file
└── LICENSE                          # MIT
```

---

## 9. Phased implementation

Each phase has clear deliverables and an exit criterion. Move forward only when met.

### Phase 0 — Setup & foundations

**Deliverables**
- Neo4j AuraDB Professional instance provisioned (Tokyo region); connection string captured.
- GCP project configured: Artifact Registry, Cloud Run, Secret Manager.
- All secrets created in Secret Manager.
- Repo scaffolded with project structure and `go mod init`.
- `mcp-go` Streamable HTTP transport wired (handler returns empty tool list).
- Config loader reads from Secret Manager with local env fallback.
- Neo4j driver initialized; schema constraints + indexes applied idempotently on boot.

**Exit criterion:** `go run ./cmd/server` boots locally; reaches AuraDB; constraints visible in Neo4j browser; MCP handshake completes from a local Claude client.

### Phase 1 — Core MCP server (base tools + deploy)

**Deliverables**
- `add_memory` (with `embedding = null`)
- `search_memory` (fulltext mode only)
- `get_memory`
- `link_memories`
- `list_recent`
- API key middleware
- Dockerfile (multi-stage, distroless)
- Cloud Run deployment with min-instances = 1
- Repository unit tests via `testcontainers-go`
- README v0 (install, configure, deploy, connect to Claude)
- ~10 real personal notes migrated for daily-use validation

**Exit criterion:** all five tools callable from Claude.ai against the deployed Cloud Run endpoint; personal notes searchable via fulltext; service has been used for at least a few days of real daily work without intervention.

### Phase 2 — Embeddings + graph traversal

**Deliverables**
- Voyage AI client wrapper + `Embedder` interface
- Embedding generation wired into `add_memory` (with retry + null fallback)
- Backfill: embed all memories with `embedding IS NULL`
- `search_memory` semantic + hybrid modes
- RRF fusion logic + unit tests
- `find_path` tool
- `find_related` tool
- Updated README with new tools and search modes
- Smoke test: every search mode + both traversal tools verified live from Claude against real data

**Exit criterion:** all seven tools callable from Claude; hybrid search returns visibly better results than fulltext alone on personal notes; `find_path` and `find_related` produce useful results on the real graph.

---

## 10. Error model

| Code | Trigger |
|---|---|
| `InvalidArgument` | Schema validation fails. |
| `MemoryNotFound` | `get_memory`, `link_memories`, `find_path`, `find_related` on missing ID. |
| `Unauthorized` | API key missing or invalid. |
| `BackendUnavailable` | Neo4j connection error (with retry). |
| `EmbeddingProviderError` | Voyage AI failed; retry, fall back (search), or warn (add). |
| `Internal` | Anything else; log fully, return generic. |

All errors: log with request ID, tool name, sanitized inputs (never full memory content in logs).

---

## 11. Observability

- Structured JSON logs to stdout (Cloud Run captures).
- `request_id` propagated through every handler.
- Cloud Run built-in metrics (request count, latency, errors).
- Cloud Logging structured log queries for ad-hoc analysis.
- No custom metrics or tracing in v1; revisit if usage outgrows it.

---

## 12. Testing strategy

- **Unit:** repository against `testcontainers-go` Neo4j (real Neo4j, throwaway).
- **Unit:** RRF fusion logic (pure function).
- **Unit:** embedder client (mock HTTP).
- **Tool handlers:** mock repository + embedder; assert parameter wiring + error mapping.
- **Manual smoke:** after each phase's deploy, exercise every in-scope tool live from Claude before declaring exit criterion met.

No load testing in v1.

---

## 13. Out of scope for v1 (v2 candidates)

- Memory delete / soft-delete (see open questions).
- Memory versioning / history.
- Bulk import / export.
- Tag rename / merge / hierarchy.
- Per-relationship-type strength / weight.
- Auto-suggested links via embedding similarity at write time.
- Cluster / community detection on the graph.
- Web UI.
- Multi-user, OAuth, sharing.
- Memory expiration / TTL.
- Streaming responses for large traversal results.

---

## 14. Resolved design decisions

The following were considered open questions during spec drafting. Decisions captured here for future reference.

| # | Decision | Reasoning |
|---|---|---|
| 1 | **Append-only in v1.** No `delete_memory` tool. Soft-delete via tombstones is a v1.1 candidate if real need emerges. | Protects against losing notes during early use while building intuition for what's worth storing. Easier to add delete later than to recover from accidental loss. |
| 2 | **Tag normalization: `lowercase + trim + drop empties`.** No slugification; no case preservation. | Standard convention; predictable behavior; avoids silent duplicate tags (`"Foo"` vs `"foo"`) and avoids mangling inputs unexpectedly (`"API/REST"` → `"apirest"`). |
| 3 | **`link_memories` uses parallel edges:** one `RELATES_TO` per unique `(from_id, to_id, relationship)` tuple. Calling with a new `relationship` string creates a new edge rather than overwriting. | Graph-first project should preserve information. "A refines B and contradicts B" is a legitimate state worth expressing. Implemented by including `relationship` in the MERGE pattern (see §4.4). |
| 4 | **Graph traversal (`find_path`, `find_related`) uses undirected `RELATES_TO` edges.** | More useful default for personal-discovery use case (`"what's related to this?"`). Relationship semantics still carry direction in the string itself. A `direction: "outgoing" \| "incoming" \| "both"` parameter is a v2 candidate if needed. |