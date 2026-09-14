package builtin

import (
	"context"
	"testing"

	"github.com/enterprise/ai-agent-go/internal/model"
)

type knowledgePipelineStub struct {
	query string
	topK  int
}

func (s *knowledgePipelineStub) Search(_ context.Context, query string, topK int) ([]model.Reference, error) {
	s.query = query
	s.topK = topK
	return []model.Reference{{DocID: "doc-1", Content: "internal fact"}}, nil
}

func TestKnowledgeSearchReturnsStructuredReferences(t *testing.T) {
	pipeline := &knowledgePipelineStub{}
	result, err := NewKnowledgeSearchTool(pipeline, 5).Execute(context.Background(), `{"query":"部署方式","top_k":8}`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || pipeline.query != "部署方式" || pipeline.topK != 8 || len(result.References) != 1 || result.References[0].DocID != "doc-1" {
		t.Fatalf("result=%+v query=%q top_k=%d", result, pipeline.query, pipeline.topK)
	}
}
