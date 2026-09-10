package vectordb

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/entity"

	"github.com/enterprise/ai-agent-go/internal/config"
)

func TestParseMetricType(t *testing.T) {
	tests := []struct {
		input string
		want  entity.MetricType
	}{
		{"", entity.COSINE},
		{"cosine", entity.COSINE},
		{" L2 ", entity.L2},
		{"ip", entity.IP},
	}
	for _, tt := range tests {
		got, err := parseMetricType(tt.input)
		if err != nil || got != tt.want {
			t.Fatalf("parseMetricType(%q) = %q, %v; want %q", tt.input, got, err, tt.want)
		}
	}
	if _, err := parseMetricType("HAMMING"); err == nil {
		t.Fatal("unsupported metric should fail")
	}
}

func TestMilvusIntegration(t *testing.T) {
	if os.Getenv("MILVUS_INTEGRATION") != "1" {
		t.Skip("set MILVUS_INTEGRATION=1 to run against a real Milvus")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	client, err := NewMilvusClient(config.MilvusConfig{
		Addr:           envOr("APP_MILVUS_ADDR", "localhost:19530"),
		Database:       "default",
		CollectionName: "agentgo_integration_test",
		Dimension:      4,
		MetricType:     "COSINE",
		ConnectTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	id := uuid.NewString()
	record := VectorRecord{ID: id, Content: "milvus integration", Embedding: []float32{1, 0, 0, 0}, Metadata: map[string]string{"kind": "test"}}
	if err := client.Insert(ctx, "", []VectorRecord{record}); err != nil {
		t.Fatal(err)
	}
	results, err := client.Search(ctx, "", []float32{1, 0, 0, 0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ID != id || results[0].Metadata["kind"] != "test" {
		t.Fatalf("unexpected search result: %+v", results)
	}
	if err := client.Delete(ctx, "", []string{id}); err != nil {
		t.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func TestMetadataAt(t *testing.T) {
	// The SDK JSON column returns []byte for each row.
	jsonColumn := column.NewColumnJSONBytes(metadataField, [][]byte{[]byte(`{"doc_id":"doc-1"}`)})
	got, err := metadataAt(jsonColumn, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got["doc_id"] != "doc-1" {
		t.Fatalf("unexpected metadata: %#v", got)
	}
}
