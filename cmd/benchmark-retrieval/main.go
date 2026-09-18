// Command benchmark-retrieval evaluates the three live retrieval modes, before answer generation.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enterprise/ai-agent-go/internal/benchmarkmeta"
	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/database"
	"github.com/enterprise/ai-agent-go/internal/embedding"
	"github.com/enterprise/ai-agent-go/internal/etl"
	"github.com/enterprise/ai-agent-go/internal/rag"
	"github.com/enterprise/ai-agent-go/internal/vectordb"
	"go.uber.org/zap"
)

type query struct {
	ID             string   `json:"id"`
	Query          string   `json:"query"`
	RelevantTitles []string `json:"relevant_titles"`
	TopK           int      `json:"top_k"`
}
type result struct {
	ID          string   `json:"id"`
	Mode        string   `json:"mode"`
	DocIDs      []string `json:"doc_ids,omitempty"`
	Recall      float64  `json:"recall_at_k"`
	Precision   float64  `json:"precision_at_k"`
	MRR         float64  `json:"mrr_at_k"`
	NDCG        float64  `json:"ndcg_at_k"`
	LatencyMS   float64  `json:"latency_ms"`
	Error       string   `json:"error,omitempty"`
	Unavailable []string `json:"unavailable,omitempty"`
}

func main() {
	dataset := flag.String("dataset", "benchmarks/datasets/retrieval.example.jsonl", "query labels")
	corpus := flag.String("corpus", "benchmarks/datasets/corpus.example.jsonl", "corpus snapshot metadata")
	output := flag.String("output", "", "JSON report path")
	backend := flag.String("backend", "real", "real or stub model backend")
	configVersion := flag.String("config-version", "config.yaml", "configuration version")
	flag.Parse()
	cfg, err := config.Load("")
	must(err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pg, err := database.NewPostgresClient(ctx, cfg.Postgres)
	must(err)
	defer pg.Close()
	milvus, err := vectordb.NewMilvusClient(cfg.Milvus)
	must(err)
	defer milvus.Close()
	embed, err := embedding.NewClient(cfg.Embedding)
	must(err)
	retriever := rag.NewRetriever(milvus, embed, nil, cfg.RAG.ScoreThreshold, zap.NewNop(), pg)
	file, err := os.Open(*dataset)
	must(err)
	defer file.Close()
	var all []result
	scan := bufio.NewScanner(file)
	for scan.Scan() {
		var q query
		must(json.Unmarshal(scan.Bytes(), &q))
		if q.TopK <= 0 {
			q.TopK = 5
		}
		wanted := make([]string, len(q.RelevantTitles))
		for i, title := range q.RelevantTitles {
			wanted[i] = etl.DocumentID("text", title)
		}
		for _, mode := range []struct {
			name  string
			value rag.RetrievalMode
		}{{"vector", rag.ModeVector}, {"keyword", rag.ModeKeyword}, {"hybrid", rag.ModeHybrid}} {
			start := time.Now()
			refs, err := retriever.RetrieveWithMode(ctx, q.Query, q.TopK, mode.value)
			entry := result{ID: q.ID, Mode: mode.name, LatencyMS: float64(time.Since(start).Microseconds()) / 1000}
			if err != nil {
				entry.Error = err.Error()
				entry.Unavailable = []string{"ranking_metrics: retrieval failed"}
			} else {
				for _, ref := range refs {
					entry.DocIDs = append(entry.DocIDs, ref.DocID)
				}
				score(&entry, wanted, q.TopK)
			}
			all = append(all, entry)
		}
	}
	must(scan.Err())
	metadata := benchmarkmeta.Collect(*dataset, *backend, "retrieval", cfg.Embedding.Model, "", *configVersion)
	metadata.CorpusSHA256 = benchmarkmeta.HashFile(*corpus)
	if metadata.CorpusSHA256 == "" {
		must(fmt.Errorf("cannot hash corpus %s", *corpus))
	}
	report := map[string]any{"metadata": metadata, "results": all}
	encoded, err := json.MarshalIndent(report, "", "  ")
	must(err)
	fmt.Println(string(encoded))
	if *output != "" {
		must(os.MkdirAll(filepath.Dir(*output), 0o755))
		must(os.WriteFile(*output, append(encoded, '\n'), 0644))
	}
}
func score(r *result, relevant []string, k int) {
	if len(relevant) == 0 {
		return
	}
	set := map[string]bool{}
	for _, id := range relevant {
		set[id] = true
	}
	hits, dcg := 0, 0.0
	seen := map[string]bool{}
	for i, id := range r.DocIDs {
		if i >= k {
			break
		}
		if !set[id] || seen[id] {
			continue
		}
		seen[id] = true
		hits++
		if r.MRR == 0 {
			r.MRR = 1 / float64(i+1)
		}
		dcg += 1 / math.Log2(float64(i+2))
	}
	r.Recall = float64(hits) / float64(len(set))
	r.Precision = float64(hits) / float64(k)
	ideal := 0.0
	for i := 0; i < len(set) && i < k; i++ {
		ideal += 1 / math.Log2(float64(i+2))
	}
	if ideal > 0 {
		r.NDCG = dcg / ideal
	}
}
func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, strings.TrimSpace(err.Error()))
		os.Exit(2)
	}
}
