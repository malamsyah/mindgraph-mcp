package mcp

import (
	"testing"

	"github.com/malamsyah/mindgraph-mcp/internal/memory"
)

func TestSelectSuggestedLinks(t *testing.T) {
	hits := []memory.SearchHit{
		{ID: "self", Score: 1.0},
		{ID: "a", Score: 0.92},
		{ID: "b", Score: 0.81},
		{ID: "c", Score: 0.70}, // below 0.75 threshold
		{ID: "d", Score: 0.78},
	}

	tests := []struct {
		name      string
		selfID    string
		threshold float64
		k         int
		wantIDs   []string
	}{
		{
			name:      "filters self, drops below-threshold, truncates to k",
			selfID:    "self",
			threshold: 0.75,
			k:         5,
			wantIDs:   []string{"a", "b", "d"},
		},
		{
			name:      "top-k truncation",
			selfID:    "self",
			threshold: 0.0,
			k:         2,
			wantIDs:   []string{"a", "b"},
		},
		{
			name:      "k=0 returns nil",
			selfID:    "self",
			threshold: 0.0,
			k:         0,
			wantIDs:   nil,
		},
		{
			name:      "threshold above all scores returns nil (not empty slice)",
			selfID:    "self",
			threshold: 0.99,
			k:         5,
			wantIDs:   nil,
		},
		{
			name:      "self id absent from hits — no special filtering",
			selfID:    "missing",
			threshold: 0.75,
			k:         5,
			wantIDs:   []string{"self", "a", "b", "d"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := selectSuggestedLinks(hits, tc.selfID, tc.threshold, tc.k)
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("got %d hits, want %d (%+v)", len(got), len(tc.wantIDs), got)
			}
			for i, want := range tc.wantIDs {
				if got[i].ID != want {
					t.Errorf("got[%d].ID = %q, want %q", i, got[i].ID, want)
				}
			}
		})
	}
}

func TestSelectSuggestedLinks_EmptyInput(t *testing.T) {
	if got := selectSuggestedLinks(nil, "self", 0.5, 5); got != nil {
		t.Errorf("expected nil for empty input, got %+v", got)
	}
}
