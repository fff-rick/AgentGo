package rag

import (
	"context"
	"testing"

	"github.com/enterprise/ai-agent-go/internal/model"
)

type pipelineRetrieverStub struct{}

func (pipelineRetrieverStub) Retrieve(context.Context, string, int) ([]model.Reference, error) {
	return []model.Reference{{DocID: "before-1"}, {DocID: "before-2"}}, nil
}

type pipelineRerankerStub struct{ called bool }

func (s *pipelineRerankerStub) Rerank(_ context.Context, _ string, _ []model.Reference) ([]model.Reference, error) {
	s.called = true
	return []model.Reference{{DocID: "reranked-1"}, {DocID: "reranked-2"}}, nil
}

func TestPipelineReranksAndAppliesTopK(t *testing.T) {
	reranker := &pipelineRerankerStub{}
	refs, err := NewPipeline(pipelineRetrieverStub{}, reranker, true).Search(context.Background(), "query", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reranker.called || len(refs) != 1 || refs[0].DocID != "reranked-1" {
		t.Fatalf("refs=%+v reranker_called=%v", refs, reranker.called)
	}
}
