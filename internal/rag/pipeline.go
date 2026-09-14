package rag

import (
	"context"
	"fmt"
	"strings"

	"github.com/enterprise/ai-agent-go/internal/model"
)

type pipelineRetriever interface {
	Retrieve(context.Context, string, int) ([]model.Reference, error)
}

type pipelineReranker interface {
	Rerank(context.Context, string, []model.Reference) ([]model.Reference, error)
}

// Pipeline owns the complete knowledge retrieval path. Query rewriting happens
// before this boundary when AgentLoop constructs the knowledge_search arguments.
type Pipeline struct {
	retriever    pipelineRetriever
	reranker     pipelineReranker
	enableRerank bool
}

func NewPipeline(retriever pipelineRetriever, reranker pipelineReranker, enableRerank bool) *Pipeline {
	return &Pipeline{retriever: retriever, reranker: reranker, enableRerank: enableRerank}
}

func (p *Pipeline) Search(ctx context.Context, query string, topK int) ([]model.Reference, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("检索问题不能为空")
	}
	if topK <= 0 {
		return nil, fmt.Errorf("top_k 必须大于 0")
	}
	refs, err := p.retriever.Retrieve(ctx, query, topK)
	if err != nil {
		return nil, err
	}
	if p.enableRerank && p.reranker != nil {
		refs, err = p.reranker.Rerank(ctx, query, refs)
		if err != nil {
			return nil, err
		}
	}
	if len(refs) > topK {
		refs = refs[:topK]
	}
	return refs, nil
}
