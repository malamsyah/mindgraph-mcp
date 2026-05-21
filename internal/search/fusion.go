package search

import (
	"sort"

	"github.com/malamsyah/mindgraph-mcp/internal/memory"
)

// Fuse combines multiple ranked SearchHit lists using Reciprocal Rank Fusion:
//
//	score(d) = sum over lists of 1 / (k + rank_in_list(d))
//
// rank is 1-indexed. Documents present in only one list still get a score,
// but documents present in multiple lists are ranked higher because their
// scores add. k=60 is the value popularized by Cormack et al. (2009) and
// matches SPEC §4.2.
//
// Lists that are nil/empty are skipped. The returned slice has length
// min(limit, len(unique documents)). Ties break by ID ascending for stable
// output.
func Fuse(lists [][]memory.SearchHit, k int, limit int) []memory.SearchHit {
	if k <= 0 {
		k = 60
	}
	if limit <= 0 {
		limit = 10
	}

	type entry struct {
		hit   memory.SearchHit
		score float64
	}
	combined := make(map[string]*entry)

	for _, list := range lists {
		for rank, hit := range list {
			contribution := 1.0 / float64(k+rank+1)
			if existing, ok := combined[hit.ID]; ok {
				existing.score += contribution
				continue
			}
			combined[hit.ID] = &entry{hit: hit, score: contribution}
		}
	}

	out := make([]memory.SearchHit, 0, len(combined))
	for _, e := range combined {
		h := e.hit
		h.Score = e.score
		out = append(out, h)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})

	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
