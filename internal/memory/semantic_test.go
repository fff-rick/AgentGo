package memory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/llm"
	"github.com/enterprise/ai-agent-go/internal/model"
	"github.com/enterprise/ai-agent-go/internal/vectordb"
)

type embeddingStub struct{}

func (embeddingStub) Embed(context.Context, string) ([]float32, error)          { return []float32{1, 0}, nil }
func (embeddingStub) EmbedBatch(context.Context, []string) ([][]float32, error) { return nil, nil }
func (embeddingStub) Healthy(context.Context) bool                              { return true }

type filteredVectorsStub struct {
	filter  string
	results []vectordb.SearchResult
}

func (*filteredVectorsStub) Insert(context.Context, string, []vectordb.VectorRecord) error {
	return nil
}
func (v *filteredVectorsStub) SearchWithFilter(_ context.Context, _ string, _ []float32, _ int, filter string) ([]vectordb.SearchResult, error) {
	v.filter = filter
	return v.results, nil
}

func TestSemanticSearchPushesUserFilterToMilvus(t *testing.T) {
	vectors := &filteredVectorsStub{results: []vectordb.SearchResult{{Content: "prefers Go", Metadata: map[string]any{"user_id": "user-1", "kind": "preference"}}}}
	store := NewSemanticStore(vectors, embeddingStub{}, "semantic_memory_v1")
	items, err := store.Search(context.Background(), "user-1", "language", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !strings.Contains(vectors.filter, `metadata["user_id"]`) || !strings.Contains(vectors.filter, `"user-1"`) {
		t.Fatalf("items=%+v filter=%q", items, vectors.filter)
	}
}

type extractionClient struct{ response string }

func (c extractionClient) Chat(context.Context, *model.LLMRequest) (*model.LLMResponse, error) {
	return &model.LLMResponse{Content: c.response}, nil
}
func (extractionClient) ChatStream(context.Context, *model.LLMRequest) (<-chan llm.StreamEvent, error) {
	return nil, nil
}
func (extractionClient) Name() string                 { return "extractor" }
func (extractionClient) Healthy(context.Context) bool { return true }

type semanticStub struct {
	saved   []model.MemoryItem
	saveErr error
}

func (*semanticStub) Search(context.Context, string, string, int) ([]model.MemoryItem, error) {
	return nil, nil
}
func (s *semanticStub) Save(_ context.Context, item model.MemoryItem) error {
	s.saved = append(s.saved, item)
	return s.saveErr
}

func TestExtractorRejectsMalformedJSONAndReturnsSaveFailure(t *testing.T) {
	newExtractor := func(response string, semantic *semanticStub) *Extractor {
		client := extractionClient{response: response}
		router := llm.NewRouter(map[string]llm.Client{"extractor": client}, []config.ModelConfig{{Name: "extractor"}}, config.CBConfig{FailureThreshold: 3, SuccessThreshold: 1})
		return NewExtractor(router, semantic, 0, 0.7, 5)
	}
	if err := newExtractor("not-json", &semanticStub{}).ExtractAndSave(context.Background(), "user-1", "session", "q", "a"); err == nil {
		t.Fatal("非法 JSON 应返回错误")
	}
	saveErr := errors.New("milvus unavailable")
	err := newExtractor(`{"memories":[{"kind":"fact","content":"长期事实","importance":0.9}]}`, &semanticStub{saveErr: saveErr}).ExtractAndSave(context.Background(), "user-1", "session", "q", "a")
	if !errors.Is(err, saveErr) {
		t.Fatalf("err=%v, want save failure", err)
	}
}

func TestExtractorValidatesAndDeduplicatesCandidates(t *testing.T) {
	client := extractionClient{response: `{"memories":[{"kind":"preference","content":"喜欢 Go","importance":0.9},{"kind":"fact","content":"临时事实","importance":0.2},{"kind":"invalid","content":"坏类型","importance":1}]}`}
	router := llm.NewRouter(map[string]llm.Client{"extractor": client}, []config.ModelConfig{{Name: "extractor"}}, config.CBConfig{FailureThreshold: 3, SuccessThreshold: 1})
	semantic := &semanticStub{}
	extractor := NewExtractor(router, semantic, 0, 0.7, 5)
	if err := extractor.ExtractAndSave(context.Background(), "user-1", "session-1", "question", "answer"); err != nil {
		t.Fatal(err)
	}
	if len(semantic.saved) != 1 || semantic.saved[0].Kind != model.MemoryPreference || semantic.saved[0].ID == "" {
		t.Fatalf("saved=%+v", semantic.saved)
	}
	firstID := semantic.saved[0].ID
	if err := extractor.ExtractAndSave(context.Background(), "user-1", "session-2", "question", "answer"); err != nil {
		t.Fatal(err)
	}
	if semantic.saved[1].ID != firstID {
		t.Fatalf("稳定记忆 ID 不一致: %q != %q", semantic.saved[1].ID, firstID)
	}
}

func TestExtractorRequiresLiteralUserEvidence(t *testing.T) {
	client := extractionClient{response: `{"memories":[{"kind":"fact","topic":"theme","content":"用户喜欢深色主题","evidence":"助手说用户喜欢深色主题","importance":0.9,"confidence":0.9}]}`}
	router := llm.NewRouter(map[string]llm.Client{"extractor": client}, []config.ModelConfig{{Name: "extractor"}}, config.CBConfig{FailureThreshold: 3, SuccessThreshold: 1})
	semantic := &semanticStub{}
	extractor := NewExtractor(router, semantic, 0, 0.7, 5)
	extractor.RequireEvidence()
	if err := extractor.ExtractAndSave(context.Background(), "user-1", "session", "请帮我查询天气", "助手说用户喜欢深色主题"); err != nil {
		t.Fatal(err)
	}
	if len(semantic.saved) != 0 {
		t.Fatalf("assistant claim was stored: %+v", semantic.saved)
	}
}
