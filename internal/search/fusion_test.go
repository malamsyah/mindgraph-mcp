package search

import (
	"testing"

	"github.com/malamsyah/mindgraph-mcp/internal/memory"
)

func hits(ids ...string) []memory.SearchHit {
	out := make([]memory.SearchHit, len(ids))
	for i, id := range ids {
		out[i] = memory.SearchHit{ID: id, Content: id}
	}
	return out
}

func TestFuse_SingleListPreservesOrder(t *testing.T) {
	out := Fuse([][]memory.SearchHit{hits("a", "b", "c")}, 60, 10)
	if len(out) != 3 || out[0].ID != "a" || out[1].ID != "b" || out[2].ID != "c" {
		t.Errorf("got %+v", idsOf(out))
	}
}

func TestFuse_OverlapBoostsScore(t *testing.T) {
	// "b" appears in both lists ⇒ should outrank "a" and "c"
	// which appear in only one list at the same rank.
	out := Fuse([][]memory.SearchHit{
		hits("a", "b", "x"),
		hits("c", "b", "y"),
	}, 60, 10)
	if out[0].ID != "b" {
		t.Errorf("expected 'b' first due to overlap, got %v", idsOf(out))
	}
}

func TestFuse_LimitTruncates(t *testing.T) {
	out := Fuse([][]memory.SearchHit{hits("a", "b", "c", "d")}, 60, 2)
	if len(out) != 2 {
		t.Errorf("expected len 2, got %d", len(out))
	}
}

func TestFuse_TieBreaksByIDAscending(t *testing.T) {
	// Both lists identical → every entry has identical score; tie-break by ID.
	out := Fuse([][]memory.SearchHit{hits("b", "a"), hits("b", "a")}, 60, 10)
	// "b" and "a" both rank 1 then 2 in each list, so:
	//   b: 2 * 1/61 = 2/61
	//   a: 2 * 1/62 = 2/62
	// b > a regardless of tie-break. Add a real tie.
	tie := Fuse([][]memory.SearchHit{
		{{ID: "z", Content: "z"}, {ID: "a", Content: "a"}},
		{{ID: "a", Content: "a"}, {ID: "z", Content: "z"}},
	}, 60, 10)
	// scores are equal — tie-break by id ascending.
	if tie[0].ID != "a" || tie[1].ID != "z" {
		t.Errorf("expected a,z (ascending tie-break), got %v", idsOf(tie))
	}
	_ = out
}

func TestFuse_EmptyLists(t *testing.T) {
	out := Fuse(nil, 60, 10)
	if len(out) != 0 {
		t.Errorf("expected empty, got %v", out)
	}
	out = Fuse([][]memory.SearchHit{{}, {}}, 60, 10)
	if len(out) != 0 {
		t.Errorf("expected empty, got %v", out)
	}
}

func TestFuse_RewritesScoreToFusion(t *testing.T) {
	out := Fuse([][]memory.SearchHit{hits("a")}, 60, 10)
	want := 1.0 / float64(60+1) // k+rank where rank=1
	if out[0].Score != want {
		t.Errorf("score = %f, want %f", out[0].Score, want)
	}
}

func idsOf(hs []memory.SearchHit) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.ID
	}
	return out
}
