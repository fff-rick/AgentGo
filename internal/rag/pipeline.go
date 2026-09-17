package rag

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/enterprise/ai-agent-go/internal/metrics"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/trace"
	"go.opentelemetry.io/otel/attribute"
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

func (p *Pipeline) Search(ctx context.Context, query string, topK int) (references []model.Reference, searchErr error) {
	ctx, span := trace.StartSpan(ctx, "rag.search")
	defer func() {
		if searchErr == nil {
			span.SetAttributes(attribute.Int("rag.results", len(references)))
		}
		trace.Finish(span, searchErr)
	}()
	metrics.Default.RAGRequests.Inc()
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("检索问题不能为空")
	}
	if topK <= 0 {
		return nil, fmt.Errorf("top_k 必须大于 0")
	}
	start := time.Now()
	refs, err := p.retriever.Retrieve(ctx, query, topK)
	metrics.Default.RAGRetrievalDuration.Observe(metrics.Seconds(start))
	if err != nil {
		return nil, err
	}
	if p.enableRerank && p.reranker != nil {
		start = time.Now()
		refs, err = p.reranker.Rerank(ctx, query, refs)
		metrics.Default.RAGRerankDuration.Observe(metrics.Seconds(start))
		if err != nil {
			return nil, err
		}
	}
	if len(refs) > topK {
		refs = refs[:topK]
	}
	metrics.Default.RAGResults.Observe(float64(len(refs)))
	return refs, nil
}
