package main

import "testing"

func TestScoreAtK(t *testing.T) {
	r := result{DocIDs: []string{"x", "b", "a"}}
	score(&r, []string{"a", "b"}, 2)
	if r.Recall != .5 || r.Precision != .5 || r.MRR != .5 || r.NDCG <= 0 || r.NDCG >= 1 {
		t.Fatalf("score=%+v", r)
	}
}
