package rag

import (
	"testing"

	"github.com/enterprise/ai-agent-go/internal/model"
)

func TestApplyRerankScoresNormalizesAndFilters(t *testing.T) {
	refs := []model.Reference{
		{DocID: "low", Score: 0.9},
		{DocID: "high", Score: 0.8},
		{DocID: "clamped", Score: 0.7},
	}
	scores := []rerankScore{
		{Index: 0, Score: 2.5},
		{Index: 1, Score: 8},
		{Index: 2, Score: 12},
	}

	got := applyRerankScores(refs, scores, 0.7)
	if len(got) != 2 {
		t.Fatalf("applyRerankScores() returned %d refs, want 2", len(got))
	}
	if got[0].DocID != "clamped" || got[0].Score != 1 {
		t.Fatalf("first ref = %#v, want clamped score 1", got[0])
	}
	if got[1].DocID != "high" || got[1].Score != 0.8 {
		t.Fatalf("second ref = %#v, want normalized score 0.8", got[1])
	}
}

func TestApplyRerankScoresExcludesUnscoredReferences(t *testing.T) {
	refs := []model.Reference{
		{DocID: "scored", Score: 0.8},
		{DocID: "omitted", Score: 0.9},
	}

	got := applyRerankScores(refs, []rerankScore{{Index: 0, Score: 8}}, 0.5)
	if len(got) != 1 || got[0].DocID != "scored" {
		t.Fatalf("applyRerankScores() = %#v, want only explicitly scored ref", got)
	}
}
