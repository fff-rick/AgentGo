package main

import "math"

type rankingMetrics struct {
	Recall    float64 `json:"recall_at_k"`
	Precision float64 `json:"precision_at_k"`
	MRR       float64 `json:"mrr_at_k"`
	NDCG      float64 `json:"ndcg_at_k"`
}

// rankScores treats the public response's ordered references as answer citations.
// Direct retriever rankings are evaluated separately, before answer generation.
func rankScores(relevant, ranked []string) *rankingMetrics {
	if len(relevant) == 0 {
		return nil
	}
	wanted := make(map[string]bool, len(relevant))
	for _, id := range relevant {
		wanted[id] = true
	}
	seen := make(map[string]bool, len(ranked))
	hits, reciprocal, dcg := 0, 0.0, 0.0
	for i, id := range ranked {
		if !wanted[id] || seen[id] {
			continue
		}
		seen[id] = true
		hits++
		if reciprocal == 0 {
			reciprocal = 1 / float64(i+1)
		}
		dcg += 1 / math.Log2(float64(i+2))
	}
	ideal := 0.0
	for i := 0; i < len(relevant) && i < len(ranked); i++ {
		ideal += 1 / math.Log2(float64(i+2))
	}
	out := &rankingMetrics{Recall: float64(hits) / float64(len(wanted)), MRR: reciprocal}
	if len(ranked) > 0 {
		out.Precision = float64(hits) / float64(len(ranked))
	}
	if ideal > 0 {
		out.NDCG = dcg / ideal
	}
	return out
}
