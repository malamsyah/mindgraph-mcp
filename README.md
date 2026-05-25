# mindgraph-mcp

A graph-backed memory MCP server for Claude. Cross-session memory store with semantic search, full-text search, and graph traversal — accessible to any MCP client (Claude.ai, Claude Code, etc.) via Streamable HTTP.

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.22%2B-00ADD8.svg)](https://go.dev)
[![MCP](https://img.shields.io/badge/MCP-2025--03--26-7C3AED.svg)](https://modelcontextprotocol.io)
[![Status](https://img.shields.io/badge/status-alpha-orange.svg)](#status)

---

## Overview

A short video explaining what mindgraph is, at a high level:

<video src="https://storage.googleapis.com/mindgraph-public-assets/mindgraph_full.mp4" controls width="720"></video>

> If the embedded player doesn't render, watch it here: [mindgraph_full.mp4](https://storage.googleapis.com/mindgraph-public-assets/mindgraph_full.mp4)

## Why mindgraph-mcp

Claude's native memory works well for most use cases. `mindgraph-mcp` is for the cases it doesn't cover:

- **You want memory you can query, traverse, and inspect** — not an opaque store the model maintains internally.
- **You want relationships between memories to be first-class** — "X refines Y", "Z contradicts W" — and to be able to follow those connections at search time.
- **You want hybrid retrieval** — full-text + semantic + tag-filtered, fused at query time, not just one mode.
- **You want it to be yours** — self-hosted, your database, your embeddings, your data.

It's a knowledge graph + vector store, served over MCP, designed so Claude can read and write it during ordinary conversations without you copy-pasting context.

## How it differs from alternatives

| | mindgraph-mcp | Native Claude memory | Vector-only memory MCPs | Notion / Obsidian |
|---|---|---|---|---|
| Graph relationships | ✅ First-class | ❌ | ❌ (mostly flat) | ⚠️ Backlinks only |
| Semantic search | ✅ | ✅ (opaque) | ✅ | ⚠️ Limited |
| Full-text search | ✅ | ❌ | ⚠️ Often missing | ✅ |
| Hybrid (RRF fusion) | ✅ | ❌ | ❌ | ❌ |
| Traversal queries | ✅ `find_path`, `find_related` | ❌ | ❌ | ❌ |
| Self-hosted | ✅ | ❌ | ⚠️ Varies | ⚠️ Obsidian |
| MCP-accessible from any client | ✅ | ❌ (Claude-only) | ✅ | ⚠️ Via plugins |

## Status

**Alpha.** Built for single-user personal use. APIs are stable as of v0.1.0 but may change before v1.0.0. Not production-hardened for multi-tenant or high-traffic scenarios.

See [SPEC.md](./SPEC.md) for the full design specification.

## Features

- **18 MCP tools** for memory CRUD, search, graph traversal, enumeration, embedding recovery, deletion, tag management, relationship removal, and code-context linking — see [tool reference](#tool-reference).
- **Three search modes**: `fulltext`, `semantic`, `hybrid` (RRF fusion, default).
- **Graph traversal**: shortest path between memories; n-hop neighborhood queries.
- **Streamable HTTP MCP transport** — remote-deployable, accessible from any compliant MCP client.
- **API key authentication** — simple bearer token; single-user.
- **Built on Neo4j** — battle-tested graph database with native vector index since 5.13.
- **Voyage AI embeddings** — Anthropic-recommended `voyage-3-large` for high-quality semantic retrieval.

## Architecture

```
                 ┌──────────────────┐
   Claude.ai ───▶│                  │
                 │   mindgraph      │       ┌──────────────────────┐
   Claude Code ─▶│  (Cloud Run)     │──────▶│  Neo4j AuraDB        │
                 │  Go + mcp-go     │ bolt+s│  Professional         │
   Other MCP ───▶│  Streamable HTTP │       └──────────────────────┘
   clients       │                  │
                 │                  │       ┌──────────────────────┐
                 │                  │──────▶│   Voyage AI          │
                 └──────────────────┘ https │  (voyage-3-large)    │
                         ▲                  └──────────────────────┘
                         │ API key
                         │ (Authorization: Bearer)
```

## Tool reference

All tools are accessed via the MCP `tools/call` method. Inputs and outputs are JSON.

### `add_memory`

Create a new memory with optional tags. Embedding is generated automatically. By default the response includes `suggested_links` — semantically similar existing memories you can wire up via `link_memories`, which prevents the graph from drifting into disconnected islands.

**Input**
```json
{
  "content": "Circuit breaker pattern: open the circuit when downstream errors exceed threshold to prevent cascading failures. Half-open state retries periodically to detect recovery.",
  "tags": ["design-patterns", "reliability", "distributed-systems"],
  "suggest_links": true
}
```

- `suggest_links` (default `true`) — set false to skip the post-write semantic search if you already know what to link.

**Output**
```json
{
  "id": "01HXY...",
  "content": "Circuit breaker pattern: open the circuit when downstream errors...",
  "created_at": "2026-05-20T03:14:00Z",
  "suggested_links": [
    { "id": "01HXZ...", "content": "Bulkhead pattern: isolate failure...", "updated_at": "...", "score": 0.87 },
    { "id": "01HXW...", "content": "Retry with jitter...", "updated_at": "...", "score": 0.79 }
  ]
}
```

Suggestions are filtered by cosine similarity ≥ `SUGGEST_LINKS_THRESHOLD` (default 0.75) and capped at 5. The newly-created memory is excluded from its own suggestions. Field is omitted when no suggestions clear the threshold or when `suggest_links: false`.

### `search_memory`

Search memories by full-text, semantic, or hybrid mode. Optional tag filter.

**Input**
```json
{
  "query": "patterns for handling downstream failures",
  "mode": "hybrid",
  "tags": ["reliability"],
  "limit": 10
}
```

`mode` is one of `fulltext`, `semantic`, `hybrid`. Default: `hybrid`. Hybrid runs both fulltext and semantic queries in parallel and fuses results with Reciprocal Rank Fusion (RRF, k=60).

### `get_memory`

Fetch a single memory by ID, with its tags, incoming/outgoing relationships, and any code references.

**Input**
```json
{ "id": "01HXY..." }
```

**Output**
```json
{
  "id": "01HXY...",
  "content": "...",
  "created_at": "...",
  "updated_at": "...",
  "tags": ["design-patterns", "reliability"],
  "outgoing": [{ "id": "01HXZ...", "relationship": "refines" }],
  "incoming": [{ "id": "01HXW...", "relationship": "context-for" }],
  "code_refs": [
    { "repo": "github.com/foo/bar", "path": "internal/limiter.go", "sha": "abc123", "line": 42 }
  ]
}
```

`code_refs` is omitted when empty.

### `link_memories`

Create or update a typed relationship between two memories.

**Input**
```json
{
  "from_id": "01HXY...",
  "to_id":   "01HXZ...",
  "relationship": "refines"
}
```

Relationships are free-form strings. Suggested vocabulary: `refines`, `contradicts`, `follows-up`, `context-for`, `supersedes`, `references`. You can use your own.

### `list_recent`

Most recently updated memories, optionally tag-filtered.

**Input**
```json
{
  "limit": 10,
  "tags": ["reliability"]
}
```

### `add_code_ref`

Attach a code reference to a memory so it surfaces via `find_by_code` when you're working in that file. Idempotent for identical `(memory_id, repo, path, sha, line)` tuples.

**Input**
```json
{
  "memory_id": "01HXY...",
  "repo": "github.com/foo/bar",
  "path": "internal/limiter.go",
  "sha": "abc123",
  "line": 42
}
```

- `sha` (optional) — empty means "HEAD-relative" (line numbers will drift over time). Pin a sha for stable references.
- `line` (optional, default 0) — 0 means a file-level reference.

**Output**
```json
{
  "memory_id": "01HXY...",
  "repo": "github.com/foo/bar",
  "path": "internal/limiter.go",
  "sha": "abc123",
  "line": 42,
  "attached": true
}
```

Returns `MemoryNotFound` if the memory id doesn't exist.

### `find_by_code`

Find memories with at least one code reference pointing at the given `(repo, path)`, regardless of sha or line. Sorted by `updated_at` DESC. Use this to surface decision-log context when opening a file.

**Input**
```json
{ "repo": "github.com/foo/bar", "path": "internal/limiter.go", "limit": 20 }
```

**Output**
```json
{
  "repo": "github.com/foo/bar",
  "path": "internal/limiter.go",
  "hits": [
    { "id": "01HXY...", "content": "decision: rate limit at edge...", "updated_at": "...", "score": 0 }
  ]
}
```

### `list_tags`

Return every tag in the graph with its usage count, sorted by count DESC then name ASC. Useful for auditing tag vocabulary before a standardization pass.

**Output**
```json
{
  "tags": [
    { "name": "mf", "count": 8 },
    { "name": "career-strategy", "count": 6 }
  ]
}
```

### `list_memories`

Paginated list of memories ordered by UUIDv7 id (creation order). Content is truncated to a 200-character preview — call `get_memory` for the full body. Pass `next_after_id` back to fetch the next page; an empty `next_after_id` indicates the last page.

**Input**
```json
{ "after_id": "01HXY...", "limit": 50 }
```

**Output**
```json
{
  "items": [
    { "id": "01HXY...", "content_preview": "Circuit breaker pattern: ...", "updated_at": "2026-05-21T03:14:00Z" }
  ],
  "next_after_id": "01HXZ..."
}
```

### `list_relationships`

Return every distinct `RELATES_TO` label with usage count. Useful for spotting label sprawl (e.g. `motivates` vs `motivated-by`) before consolidation.

**Output**
```json
{
  "relationships": [
    { "label": "refines", "count": 12 },
    { "label": "context-for", "count": 7 }
  ]
}
```

### `find_path`

Shortest path between two memories via `RELATES_TO` edges.

**Input**
```json
{
  "from_id":  "01HXY...",
  "to_id":    "01HXW...",
  "max_hops": 4
}
```

**Output**
```json
{
  "nodes": [
    { "id": "01HXY...", "content": "..." },
    { "id": "01HXZ...", "content": "..." },
    { "id": "01HXW...", "content": "..." }
  ],
  "relationships": ["refines", "context-for"],
  "hops": 2
}
```

Returns `hops: -1` if no path exists within `max_hops`.

### `find_related`

Discover memories within N hops of a starting memory.

**Input**
```json
{
  "id": "01HXY...",
  "hops": 2,
  "relationship_filter": ["refines", "context-for"],
  "limit": 20
}
```

Returns memories sorted by graph distance (ascending). Optional `relationship_filter` restricts which edge types count.

### `reembed_memories`

Regenerate embeddings for memories whose original embedding call failed (or for the entire corpus, e.g. after switching models). The server also runs `scope=missing` once at boot; this tool exposes the same routine for on-demand recovery.

**Input**
```json
{
  "scope": "missing",
  "limit": 0
}
```

- `scope` — `missing` (default; only memories with NULL embedding) or `all` (every memory, regardless of current state).
- `id` — optional; when set, only that single memory is re-embedded and `scope` is ignored.
- `limit` — max memories to process in this call. `0` (default) means no cap.

**Output**
```json
{
  "scope": "missing",
  "processed": 12,
  "succeeded": 12,
  "failed": 0
}
```

When some memories fail, `failures` is included with per-id error messages. If the embedder is unreachable, the partial result is returned alongside the error so you can see what was completed.

### `delete_memory`

Permanently delete a memory and all of its incoming/outgoing relationships. Tag nodes are preserved (they're shared and reusable). This is a hard delete — the memory cannot be recovered.

**Input**
```json
{ "id": "01HXY..." }
```

**Output**
```json
{ "id": "01HXY...", "deleted": true }
```

Returns `MemoryNotFound` if the id doesn't exist.

### `delete_relationship`

Remove the directional `RELATES_TO` edge between two memories carrying a specific label. Use to drop inverse-pair duplicates (e.g. delete `derives-from` when `source-of` is the canonical direction) or to clear typos. Parallel edges with other labels survive.

**Input**
```json
{
  "from_id": "01HXY...",
  "to_id":   "01HXZ...",
  "relationship": "derives-from"
}
```

**Output**
```json
{
  "from_id": "01HXY...",
  "to_id":   "01HXZ...",
  "relationship": "derives-from",
  "deleted": true
}
```

Returns `RelationshipNotFound` if no such edge exists. Directional — to delete the reverse edge, swap `from_id` and `to_id`.

### `delete_tag`

Permanently delete a tag and remove it from every memory it's attached to. Memories themselves are preserved (a memory with no tags is still a valid memory).

**Input**
```json
{ "name": "deprecated" }
```

**Output**
```json
{ "name": "deprecated", "deleted": true, "memories_affected": 4 }
```

Returns `TagNotFound` if the tag doesn't exist. Names are normalized (lowercase + trim) before lookup.

### `update_tag`

Rename a tag. The new name must not already exist — to fold one tag into another, use `merge_tags`.

**Input**
```json
{ "old_name": "designpatterns", "new_name": "design-patterns" }
```

**Output**
```json
{
  "old_name": "designpatterns",
  "new_name": "design-patterns",
  "renamed": true,
  "memories_affected": 5
}
```

Returns `InvalidArgument` if `new_name` already exists.

### `merge_tags`

Fold a source tag into a target tag: every memory tagged with `source` becomes tagged with `target` (no duplicate edges), then `source` is deleted. Both tags must already exist.

**Input**
```json
{ "source": "ml", "target": "machine-learning" }
```

**Output**
```json
{
  "source": "ml",
  "target": "machine-learning",
  "merged": true,
  "memories_affected": 12
}
```

## Quick start

### Prerequisites

- Go 1.22+
- Docker (for local Neo4j) OR a Neo4j 5.13+ instance
- A [Voyage AI](https://voyageai.com) API key (for embeddings)

### Run locally with Docker Neo4j

```bash
# 1. Start a local Neo4j instance
docker run -d \
  --name mindgraph-neo4j \
  -p 7474:7474 -p 7687:7687 \
  -e NEO4J_AUTH=neo4j/devpassword \
  -e NEO4J_PLUGINS='["apoc"]' \
  neo4j:5.20-enterprise

# 2. Clone and configure
git clone https://github.com/malamsyah/mindgraph-mcp.git
cd mindgraph-mcp
cp .env.example .env
# Edit .env with your VOYAGE_API_KEY and an API key for mindgraph itself

# 3. Run
go run ./cmd/server
```

The server starts on `http://localhost:8080` by default.

### Smoke test

```bash
# List available tools
curl -X POST http://localhost:8080/mcp \
  -H "Authorization: Bearer $MINDGRAPH_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'

# Add a memory
curl -X POST http://localhost:8080/mcp \
  -H "Authorization: Bearer $MINDGRAPH_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc":"2.0","id":2,"method":"tools/call",
    "params":{
      "name":"add_memory",
      "arguments":{
        "content":"First test memory.",
        "tags":["test"]
      }
    }
  }'
```

## Configuration

`mindgraph` reads configuration from environment variables. In production, all secrets are pulled from Google Secret Manager (via env var override).

| Variable | Required | Default | Notes |
|---|---|---|---|
| `MINDGRAPH_API_KEY` | yes | — | 32+ char bearer token for app-level auth. |
| `NEO4J_URI` | yes | — | `neo4j+s://...` for AuraDB; `bolt://...` for local. |
| `NEO4J_USER` | yes | — | Usually `neo4j`. |
| `NEO4J_PASSWORD` | yes | — | Use Secret Manager in production. |
| `VOYAGE_API_KEY` | yes | — | From [Voyage AI dashboard](https://dash.voyageai.com). |
| `EMBEDDING_MODEL` | no | `voyage-3-large` | Voyage AI model name. |
| `EMBEDDING_DIMENSIONS` | no | `2048` | Must match the vector index DDL. |
| `SUGGEST_LINKS_THRESHOLD` | no | `0.75` | Min cosine similarity for `add_memory` to surface a memory as a suggested link. |
| `PORT` | no | `8080` | HTTP listen port. |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn`, `error`. |

See [`.env.example`](.env.example) for a template.

## Deployment

### Google Cloud Run + Neo4j AuraDB Professional

This is the reference deployment.

**1. Provision Neo4j AuraDB Professional**

Sign up at [neo4j.com/cloud/aura](https://neo4j.com/cloud/aura), create a Professional instance in `asia-northeast1` (or your nearest region). Capture the connection URI, user, and password.

**2. Create secrets in Google Secret Manager**

```bash
gcloud secrets create mindgraph-api-key --data-file=- < /dev/stdin
# (paste your generated API key, then Ctrl-D)

gcloud secrets create neo4j-uri --data-file=- < /dev/stdin
gcloud secrets create neo4j-user --data-file=- < /dev/stdin
gcloud secrets create neo4j-password --data-file=- < /dev/stdin
gcloud secrets create voyage-api-key --data-file=- < /dev/stdin
```

**3. Build and push the image**

```bash
PROJECT_ID=$(gcloud config get-value project)
REGION=asia-northeast1

 gcloud artifacts repositories create mindgraph \
    --repository-format=docker \
    --location=asia-northeast1

gcloud builds submit \
  --tag $REGION-docker.pkg.dev/$PROJECT_ID/mindgraph/mindgraph:latest
```

**4. Deploy to Cloud Run**

```bash
gcloud run deploy mindgraph \
  --image $REGION-docker.pkg.dev/$PROJECT_ID/mindgraph/mindgraph:latest \
  --region $REGION \
  --platform managed \
  --allow-unauthenticated \
  --min-instances 1 \
  --max-instances 4 \
  --cpu 2 \
  --memory 1Gi \
  --timeout 60s \
  --concurrency 80 \
  --set-secrets="MINDGRAPH_API_KEY=mindgraph-api-key:latest,NEO4J_URI=neo4j-uri:latest,NEO4J_USER=neo4j-user:latest,NEO4J_PASSWORD=neo4j-password:latest,VOYAGE_API_KEY=voyage-api-key:latest"
```

Cloud Run will return a URL. Use that URL when connecting Claude clients.

### Cost expectations

- **Neo4j AuraDB Professional**: ~$65/month (smallest instance)
- **Cloud Run** with min-instances=1: ~$15-20/month
- **Voyage AI** (personal use): <$1/month
- **Total**: ~$80-90/month

For lower-cost setups, swap to AuraDB Free Tier and Cloud Run scale-to-zero — this trades cold-start latency and capacity limits for near-zero cost.

## Connecting Claude clients

### Claude Code

Add `mindgraph` as a remote MCP server:

```bash
claude mcp add mindgraph \
  --url https://YOUR-DEPLOYMENT.run.app \
  --header "Authorization: Bearer YOUR_MINDGRAPH_API_KEY"
```

Then verify in any Claude Code session:

```
/mcp
```

### Claude.ai

In Settings → Connectors → Add custom connector:

- **Name**: `mindgraph`
- **URL**: your Cloud Run URL
- **Authentication**: Custom header
  - Header name: `Authorization`
  - Header value: `Bearer YOUR_MINDGRAPH_API_KEY`

### Other MCP clients

Any client supporting MCP Streamable HTTP transport should work. Point it at your Cloud Run URL with the `Authorization: Bearer <key>` header.

## Development

### Project layout

```
mindgraph-mcp/
├── cmd/server/main.go              # Entry point
├── internal/
│   ├── config/                     # Env + Secret Manager loading
│   ├── memory/                     # Neo4j repository
│   ├── embeddings/                 # Voyage AI client
│   ├── reembed/                    # Boot backfill + reembed_memories
│   ├── search/                     # RRF fusion
│   ├── mcp/                        # mcp-go server + tool handlers
│   └── auth/                       # API key middleware
├── Dockerfile
├── SPEC.md                         # Full design spec
└── README.md
```

### Running tests

```bash
# Unit tests (no external deps)
go test ./internal/search/...
go test ./internal/auth/...

# Integration tests (require Docker for testcontainers)
go test ./internal/memory/...
go test ./internal/embeddings/... # mocks Voyage AI
```

### Building locally

```bash
go build -o bin/mindgraph ./cmd/server
./bin/mindgraph
```

### Local Neo4j with full plugin set

For local dev, run Neo4j with APOC plugins so all queries work:

```bash
docker run -d \
  --name mindgraph-neo4j \
  -p 7474:7474 -p 7687:7687 \
  -e NEO4J_AUTH=neo4j/devpassword \
  -e NEO4J_PLUGINS='["apoc"]' \
  -e NEO4J_dbms_security_procedures_unrestricted='apoc.*' \
  neo4j:5.20-enterprise
```

Browse to `http://localhost:7474` to inspect data visually during development.

## Roadmap

v1 ships the eighteen tools described above. Tracked for future versions:

- **v1.1**: Memory update tool (re-embed on content change); soft-delete with tombstones (current `delete_memory` is hard-delete only).
- **v1.2**: Bulk import / export (markdown directory in, JSON dump out).
- **v1.3**: Memory versioning / history.
- **v2.0**: Per-relationship-type weights; auto-suggested links via embedding similarity at write time.
- **v2.x**: Cluster / community detection; richer traversal (paths along specific relationship types only); per-tag access control.

See [SPEC.md §13](./SPEC.md) for the full out-of-scope list.

## Contributing

This is currently a single-maintainer personal project. PRs welcome but expect slow review. Please file an issue first to discuss substantive changes — the scope is deliberately small.

For bug reports, include:
- `mindgraph` version (commit SHA is fine)
- Neo4j version
- Steps to reproduce
- Expected vs actual behavior
- Relevant log lines (with secrets redacted)

## License

[MIT](./LICENSE) © Mochammad Alamsyah

## Acknowledgments

- [Neo4j](https://neo4j.com) for the graph database and native vector index.
- [Voyage AI](https://voyageai.com) for the embedding models.
- [`mark3labs/mcp-go`](https://github.com/mark3labs/mcp-go) for the Go MCP SDK.
- [Anthropic](https://anthropic.com) for the Model Context Protocol and Claude.

---

Built by [@malamsyah](https://github.com/malamsyah). See [malamsyah.com](https://malamsyah.com) for other projects.