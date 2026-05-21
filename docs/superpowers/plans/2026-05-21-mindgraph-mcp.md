# mindgraph MCP Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a Go-based remote MCP server (`mindgraph`) that stores memories in Neo4j with Voyage AI embeddings, exposing 7 tools for CRUD, search (fulltext / semantic / hybrid), and graph traversal.

**Architecture:** Single-binary Go HTTP server using `mark3labs/mcp-go` Streamable HTTP transport. Neo4j 5.x via the official bolt driver with native vector + fulltext indexes. Voyage AI `voyage-3-large` embeddings (2048 dims). Deployed to Cloud Run with secrets from GCP Secret Manager. Application-side RRF fusion for hybrid search.

**Tech Stack:** Go 1.22+, `github.com/mark3labs/mcp-go`, `github.com/neo4j/neo4j-go-driver/v5`, `github.com/google/uuid`, `testcontainers-go`, Voyage AI API.

---

## File Structure

```
mindgraph-mcp/
├── cmd/server/main.go              # entry, wiring
├── internal/
│   ├── config/config.go            # env + Secret Manager loader
│   ├── memory/
│   │   ├── model.go                # Memory, Tag, Relationship, search types
│   │   ├── repository.go           # Neo4j CRUD + search + traversal
│   │   └── repository_test.go      # testcontainers-go
│   ├── embeddings/
│   │   ├── embedder.go             # interface
│   │   ├── voyage.go               # Voyage AI HTTP client
│   │   └── voyage_test.go          # httptest mock
│   ├── search/
│   │   ├── fusion.go               # RRF combination
│   │   └── fusion_test.go
│   ├── mcp/
│   │   ├── server.go               # mcp-go bootstrap
│   │   └── tools.go                # 7 tool handlers
│   └── auth/
│       ├── middleware.go           # API key bearer
│       └── middleware_test.go
├── Dockerfile
├── .env.example
├── go.mod
└── go.sum
```

---

## Phase 0 — Setup & foundations

### Task 0.1: Initialize Go module + .gitignore + .env.example

**Files:**
- Create: `go.mod`
- Create: `.gitignore`
- Create: `.env.example`

- [ ] Run `go mod init github.com/malamsyah/mindgraph-mcp`
- [ ] Add neo4j driver, mcp-go, uuid, joho/godotenv as deps
- [ ] Write `.gitignore` excluding `bin/`, `.env`, `*.local`, common Go artifacts
- [ ] Write `.env.example` with all variables from SPEC §6
- [ ] Commit: "chore: scaffold go module and env templates"

### Task 0.2: Config loader

**Files:**
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`

- [ ] Test: `LoadFromEnv` returns populated `Config` when all required env vars set
- [ ] Test: `LoadFromEnv` returns error when required env var missing
- [ ] Implement `Config` struct with all fields from SPEC §6
- [ ] Implement `LoadFromEnv` that reads from env, applies defaults
- [ ] Run tests; commit

### Task 0.3: Neo4j driver init + schema bootstrap

**Files:**
- Create: `internal/memory/repository.go` (skeleton)
- Create: `internal/memory/repository_test.go` (testcontainers)

- [ ] Test: `NewRepository` connects to Neo4j and applies constraints/indexes idempotently (run twice — second should not error)
- [ ] Implement `Repository` struct with `driver neo4j.DriverWithContext`
- [ ] Implement `NewRepository(ctx, uri, user, password)` 
- [ ] Implement `Bootstrap(ctx)` that applies the 5 schema statements from SPEC §3
- [ ] Implement `Close(ctx)`
- [ ] Run tests; commit

### Task 0.4: MCP server skeleton with empty tool list

**Files:**
- Create: `internal/mcp/server.go`
- Create: `cmd/server/main.go`

- [ ] Implement `mcp.NewServer(...)` returning configured `*server.MCPServer` with no tools
- [ ] Implement `main.go` that loads config, creates repo, bootstraps schema, starts Streamable HTTP server on `:PORT`
- [ ] Manually verify with `curl -X POST localhost:8080/mcp -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'`
- [ ] Commit: "feat(phase0): scaffold server with empty MCP handler"

**Exit:** `go run ./cmd/server` boots locally, applies schema, serves empty `tools/list`.

---

## Phase 1 — Core MCP server

### Task 1.1: Memory model types

**Files:**
- Create: `internal/memory/model.go`

- [ ] Define `Memory`, `Tag`, `Relationship`, `MemoryDetail`, `SearchHit`, `SearchMode` types
- [ ] Define `NormalizeTags(in []string) []string` (lowercase, trim, drop empty, dedupe)
- [ ] Unit test `NormalizeTags`
- [ ] Commit

### Task 1.2: `Repository.AddMemory`

**Files:**
- Modify: `internal/memory/repository.go`
- Modify: `internal/memory/repository_test.go`

- [ ] Test: `AddMemory` creates node with UUIDv7 id, content, timestamps, optional embedding, tags
- [ ] Test: tags are normalized
- [ ] Test: duplicate tag names merge to one `:Tag` node
- [ ] Implement using SPEC §4.1 Cypher
- [ ] Commit

### Task 1.3: `Repository.GetMemory`

**Files:**
- Modify: `internal/memory/repository.go`
- Modify: `internal/memory/repository_test.go`

- [ ] Test: returns memory + outgoing + incoming relationships + tags
- [ ] Test: returns `ErrMemoryNotFound` when id missing
- [ ] Implement using SPEC §4.3 Cypher
- [ ] Commit

### Task 1.4: `Repository.LinkMemories`

**Files:**
- Modify: `internal/memory/repository.go`
- Modify: `internal/memory/repository_test.go`

- [ ] Test: creates `RELATES_TO` with relationship property
- [ ] Test: calling twice with same triple is idempotent (MERGE)
- [ ] Test: calling with different relationship creates parallel edge
- [ ] Test: `ErrMemoryNotFound` when either side missing
- [ ] Implement using SPEC §4.4 Cypher
- [ ] Commit

### Task 1.5: `Repository.ListRecent`

**Files:**
- Modify: `internal/memory/repository.go`
- Modify: `internal/memory/repository_test.go`

- [ ] Test: returns memories ordered by `updated_at DESC`
- [ ] Test: tag filter restricts results (AND across tags)
- [ ] Test: `limit` honored
- [ ] Implement using SPEC §4.5 Cypher
- [ ] Commit

### Task 1.6: `Repository.SearchFulltext`

**Files:**
- Modify: `internal/memory/repository.go`
- Modify: `internal/memory/repository_test.go`

- [ ] Test: returns matches ordered by score DESC
- [ ] Test: tag filter restricts results
- [ ] Test: empty query returns empty result (no fulltext panic)
- [ ] Implement using SPEC §4.2 fulltext Cypher
- [ ] Commit

### Task 1.7: API key middleware

**Files:**
- Create: `internal/auth/middleware.go`
- Create: `internal/auth/middleware_test.go`

- [ ] Test: missing `Authorization` header → 401
- [ ] Test: wrong key → 401
- [ ] Test: correct key → next handler invoked
- [ ] Test: comparison is constant-time (call with prefix-match key — should still fail)
- [ ] Implement `Middleware(expectedKey string) func(http.Handler) http.Handler`
- [ ] Commit

### Task 1.8: MCP tool handlers — Phase 1 set

**Files:**
- Create: `internal/mcp/tools.go`
- Modify: `internal/mcp/server.go`

- [ ] Implement handlers for `add_memory`, `search_memory` (fulltext mode only — reject `semantic`/`hybrid` with `InvalidArgument` for now), `get_memory`, `link_memories`, `list_recent`
- [ ] Each handler validates inputs per SPEC §4, calls repository, maps errors per SPEC §10
- [ ] Register all 5 tools with the MCP server
- [ ] Commit

### Task 1.9: Wire auth middleware + handlers in main

**Files:**
- Modify: `cmd/server/main.go`

- [ ] Wrap MCP HTTP handler with auth middleware
- [ ] Add structured JSON logging (`log/slog`) with request_id
- [ ] Add graceful shutdown on SIGTERM
- [ ] Manual smoke test: `add_memory` → `get_memory` → `search_memory` → `list_recent` → `link_memories` flow via curl
- [ ] Commit

### Task 1.10: Dockerfile

**Files:**
- Create: `Dockerfile`

- [ ] Multi-stage: `golang:1.22-alpine` builder, `gcr.io/distroless/static:nonroot` runtime
- [ ] Build static binary with `CGO_ENABLED=0`
- [ ] `EXPOSE 8080`, non-root user, no shell in runtime
- [ ] Build locally with `docker build -t mindgraph .`
- [ ] Run with `docker run -p 8080:8080 --env-file .env mindgraph`
- [ ] Commit

**Exit:** All 5 tools callable from any MCP client against a locally running container; auth gate works; container builds clean.

---

## Phase 2 — Embeddings + graph traversal

### Task 2.1: Embedder interface + Voyage client

**Files:**
- Create: `internal/embeddings/embedder.go`
- Create: `internal/embeddings/voyage.go`
- Create: `internal/embeddings/voyage_test.go`

- [ ] Define `InputType` enum (`document`, `query`)
- [ ] Define `Embedder` interface: `Embed(ctx, texts []string, inputType InputType) ([][]float32, error)`
- [ ] Implement `VoyageClient` against `https://api.voyageai.com/v1/embeddings`
- [ ] Test using `httptest.NewServer`: happy path, non-200 error, malformed JSON
- [ ] Implement exponential backoff (3 attempts) for 5xx and network errors
- [ ] Commit

### Task 2.2: Wire embedding into `add_memory`

**Files:**
- Modify: `internal/mcp/tools.go`
- Modify: `internal/mcp/server.go`
- Modify: `cmd/server/main.go`

- [ ] `add_memory` handler embeds content (input type `document`) before persisting
- [ ] On Voyage failure after retries: persist with `embedding = null`, include `warning: "embedding_failed"` in response
- [ ] Commit

### Task 2.3: Backfill missing embeddings on boot

**Files:**
- Modify: `internal/memory/repository.go`
- Modify: `cmd/server/main.go`

- [ ] Add `Repository.MissingEmbeddings(ctx, limit)` and `UpdateEmbedding(ctx, id, vec)`
- [ ] Add backfill routine in `main.go` that runs once at startup, batches of 128
- [ ] Commit

### Task 2.4: `Repository.SearchSemantic`

**Files:**
- Modify: `internal/memory/repository.go`
- Modify: `internal/memory/repository_test.go`

- [ ] Test: returns memories ordered by cosine similarity
- [ ] Test: tag filter applied post-vector-search using `candidate_limit = limit * 3`
- [ ] Implement using SPEC §4.2 semantic Cypher
- [ ] Commit

### Task 2.5: RRF fusion

**Files:**
- Create: `internal/search/fusion.go`
- Create: `internal/search/fusion_test.go`

- [ ] Test: identical inputs → fused order matches input
- [ ] Test: item in both lists scored higher than item in only one
- [ ] Test: stable order on tied scores (by id)
- [ ] Implement `Fuse(lists [][]SearchHit, k int, limit int) []SearchHit`
- [ ] Commit

### Task 2.6: Wire hybrid + semantic into `search_memory`

**Files:**
- Modify: `internal/mcp/tools.go`

- [ ] Remove fulltext-only restriction
- [ ] For semantic: embed query (input type `query`), call `SearchSemantic`
- [ ] For hybrid: run fulltext + semantic in parallel via `errgroup`, fuse via `search.Fuse`, both lists requested at `limit*2`
- [ ] On Voyage failure during semantic/hybrid: fall back to fulltext with warning
- [ ] Commit

### Task 2.7: `Repository.FindPath` + tool

**Files:**
- Modify: `internal/memory/repository.go`
- Modify: `internal/memory/repository_test.go`
- Modify: `internal/mcp/tools.go`

- [ ] Test: returns shortest undirected path within `max_hops`
- [ ] Test: returns empty path with `hops=-1` when no path
- [ ] Test: rejects `max_hops` > 6 via validation
- [ ] Implement using SPEC §4.6 Cypher (interpolate validated `max_hops` int)
- [ ] Register `find_path` MCP tool
- [ ] Commit

### Task 2.8: `Repository.FindRelated` + tool

**Files:**
- Modify: `internal/memory/repository.go`
- Modify: `internal/memory/repository_test.go`
- Modify: `internal/mcp/tools.go`

- [ ] Test: returns memories within N hops, sorted by distance
- [ ] Test: relationship filter restricts edges
- [ ] Test: `limit` honored
- [ ] Implement using SPEC §4.7 Cypher
- [ ] Register `find_related` MCP tool
- [ ] Commit

**Exit:** All 7 tools callable; hybrid > fulltext on real notes; traversal works.

---

## Self-Review Checklist

- Spec sections covered: §3 schema ✓ (0.3), §4.1 ✓ (1.2), §4.2 fulltext ✓ (1.6), §4.2 semantic ✓ (2.4), §4.2 hybrid ✓ (2.5+2.6), §4.3 ✓ (1.3), §4.4 ✓ (1.4), §4.5 ✓ (1.5), §4.6 ✓ (2.7), §4.7 ✓ (2.8), §5 voyage ✓ (2.1+2.2+2.3), §6 config ✓ (0.2), §7 dockerfile ✓ (1.10), §10 errors ✓ (1.8 mapping)
- No placeholders; all Cypher and code lives in SPEC or is exact-pattern.
- Type consistency: `SearchHit` defined once in 1.1, used in 1.6/2.4/2.5/2.6.
