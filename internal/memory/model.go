package memory

import (
	"errors"
	"strings"
	"time"
)

var (
	ErrMemoryNotFound       = errors.New("memory not found")
	ErrTagNotFound          = errors.New("tag not found")
	ErrRelationshipNotFound = errors.New("relationship not found")
	ErrInvalidArgs          = errors.New("invalid arguments")
)

type SearchMode string

const (
	SearchFulltextMode SearchMode = "fulltext"
	SearchSemanticMode SearchMode = "semantic"
	SearchHybridMode   SearchMode = "hybrid"
)

// Memory is the stored knowledge unit.
type Memory struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Relationship summarises one edge end of a memory.
type Relationship struct {
	ID           string `json:"id"`
	Relationship string `json:"relationship"`
}

// MemoryDetail is the full view returned by get_memory.
type MemoryDetail struct {
	Memory
	Tags     []string       `json:"tags"`
	Outgoing []Relationship `json:"outgoing"`
	Incoming []Relationship `json:"incoming"`
}

// SearchHit is a single result from any search mode. Score is opaque between
// modes; only its ordering within a single result list is meaningful.
type SearchHit struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	UpdatedAt time.Time `json:"updated_at"`
	Score     float64   `json:"score"`
}

// PathNode is one node on a returned find_path traversal.
type PathNode struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

// PathResult is the result of find_path.
type PathResult struct {
	Nodes         []PathNode `json:"nodes"`
	Relationships []string   `json:"relationships"`
	Hops          int        `json:"hops"`
}

// RelatedMemory is one entry of a find_related result.
type RelatedMemory struct {
	ID       string `json:"id"`
	Content  string `json:"content"`
	Distance int    `json:"distance"`
}

// NormalizeTagName lowercases and trims a single tag string. Empty / whitespace
// inputs return "". Used wherever a single tag name is accepted (delete_tag,
// update_tag, merge_tags) so callers see consistent matching regardless of
// casing or surrounding whitespace.
func NormalizeTagName(in string) string {
	return strings.ToLower(strings.TrimSpace(in))
}

// NormalizeTags lowercases, trims, drops empty, and deduplicates while
// preserving first-occurrence order.
func NormalizeTags(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = NormalizeTagName(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}
