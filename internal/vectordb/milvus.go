// Package vectordb provides vector database abstractions and a Milvus implementation.
package vectordb

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/index"
	"github.com/milvus-io/milvus/client/v2/milvusclient"

	"github.com/enterprise/ai-agent-go/internal/config"
)

const (
	idField        = "id"
	contentField   = "content"
	embeddingField = "embedding"
	metadataField  = "metadata"
	maxIDLength    = 128
	maxContentLen  = 65535
)

type VectorRecord struct {
	ID        string
	Content   string
	Embedding []float32
	Metadata  map[string]string
}

type SearchResult struct {
	ID       string
	Content  string
	Score    float64
	Metadata map[string]string
}

type VectorDB interface {
	Insert(ctx context.Context, collection string, records []VectorRecord) error
	Search(ctx context.Context, collection string, vector []float32, topK int) ([]SearchResult, error)
	Delete(ctx context.Context, collection string, ids []string) error
	Close() error
	Healthy(ctx context.Context) bool
}

// MilvusClient owns a real gRPC connection and lazily creates collections that
// use AgentGo's common id/content/embedding/metadata schema.
type MilvusClient struct {
	client         *milvusclient.Client
	collectionName string
	dimension      int
	metricType     entity.MetricType

	collectionsMu sync.Mutex
	collections   map[string]struct{}
}

func NewMilvusClient(cfg config.MilvusConfig) (*MilvusClient, error) {
	if cfg.Dimension <= 0 {
		return nil, fmt.Errorf("Milvus dimension 必须大于 0")
	}
	metric, err := parseMetricType(cfg.MetricType)
	if err != nil {
		return nil, err
	}
	connectTimeout := cfg.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	client, err := milvusclient.New(ctx, &milvusclient.ClientConfig{
		Address:  cfg.Addr,
		Username: cfg.Username,
		Password: cfg.Password,
		DBName:   cfg.Database,
	})
	if err != nil {
		return nil, fmt.Errorf("Milvus 连接失败 (%s): %w", cfg.Addr, err)
	}
	c := &MilvusClient{
		client:         client,
		collectionName: cfg.CollectionName,
		dimension:      cfg.Dimension,
		metricType:     metric,
		collections:    make(map[string]struct{}),
	}
	if err := c.ensureCollection(ctx, cfg.CollectionName); err != nil {
		_ = client.Close(context.Background())
		return nil, fmt.Errorf("初始化 Milvus collection %q 失败: %w", cfg.CollectionName, err)
	}
	return c, nil
}

func (c *MilvusClient) Insert(ctx context.Context, collectionName string, records []VectorRecord) error {
	if len(records) == 0 {
		return nil
	}
	collectionName = c.resolveCollection(collectionName)
	if err := c.ensureCollection(ctx, collectionName); err != nil {
		return err
	}

	ids := make([]string, len(records))
	contents := make([]string, len(records))
	vectors := make([][]float32, len(records))
	metadata := make([][]byte, len(records))
	for i, record := range records {
		id := record.ID
		if id == "" {
			id = uuid.NewString()
		}
		if len(id) > maxIDLength {
			return fmt.Errorf("record %d id 长度 %d 超过上限 %d", i, len(id), maxIDLength)
		}
		if len(record.Content) > maxContentLen {
			return fmt.Errorf("record %d content 长度 %d 超过上限 %d bytes", i, len(record.Content), maxContentLen)
		}
		if len(record.Embedding) != c.dimension {
			return fmt.Errorf("record %d embedding 维度为 %d，期望 %d", i, len(record.Embedding), c.dimension)
		}
		encoded, err := json.Marshal(record.Metadata)
		if err != nil {
			return fmt.Errorf("record %d metadata 序列化失败: %w", i, err)
		}
		ids[i], contents[i], vectors[i], metadata[i] = id, record.Content, record.Embedding, encoded
	}

	option := milvusclient.NewColumnBasedInsertOption(collectionName,
		column.NewColumnVarChar(idField, ids),
		column.NewColumnVarChar(contentField, contents),
		column.NewColumnFloatVector(embeddingField, c.dimension, vectors),
		column.NewColumnJSONBytes(metadataField, metadata),
	)
	if _, err := c.client.Upsert(ctx, option); err != nil {
		return fmt.Errorf("Milvus upsert 失败: %w", err)
	}
	return nil
}

func (c *MilvusClient) Search(ctx context.Context, collectionName string, vector []float32, topK int) ([]SearchResult, error) {
	if len(vector) != c.dimension {
		return nil, fmt.Errorf("查询向量维度为 %d，期望 %d", len(vector), c.dimension)
	}
	if topK <= 0 {
		return nil, fmt.Errorf("topK 必须大于 0")
	}
	collectionName = c.resolveCollection(collectionName)
	if err := c.ensureCollection(ctx, collectionName); err != nil {
		return nil, err
	}

	sets, err := c.client.Search(ctx, milvusclient.NewSearchOption(
		collectionName,
		topK,
		[]entity.Vector{entity.FloatVector(vector)},
	).WithANNSField(embeddingField).
		WithOutputFields(contentField, metadataField).
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		return nil, fmt.Errorf("Milvus search 失败: %w", err)
	}
	if len(sets) == 0 {
		return nil, nil
	}

	set := sets[0]
	contentColumn := set.GetColumn(contentField)
	metadataColumn := set.GetColumn(metadataField)
	results := make([]SearchResult, 0, set.ResultCount)
	for i := 0; i < set.ResultCount; i++ {
		id, err := stringAt(set.IDs, i)
		if err != nil {
			return nil, fmt.Errorf("解析 search id 失败: %w", err)
		}
		content, err := stringAt(contentColumn, i)
		if err != nil {
			return nil, fmt.Errorf("解析 search content 失败: %w", err)
		}
		meta, err := metadataAt(metadataColumn, i)
		if err != nil {
			return nil, fmt.Errorf("解析 search metadata 失败: %w", err)
		}
		score := float64(set.Scores[i])
		if c.metricType == entity.L2 {
			score = 1 / (1 + score)
		}
		results = append(results, SearchResult{ID: id, Content: content, Score: score, Metadata: meta})
	}
	return results, nil
}

func (c *MilvusClient) Delete(ctx context.Context, collectionName string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	collectionName = c.resolveCollection(collectionName)
	if err := c.ensureCollection(ctx, collectionName); err != nil {
		return err
	}
	if _, err := c.client.Delete(ctx, milvusclient.NewDeleteOption(collectionName).WithStringIDs(idField, ids)); err != nil {
		return fmt.Errorf("Milvus delete 失败: %w", err)
	}
	return nil
}

func (c *MilvusClient) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.client.Close(ctx)
}

func (c *MilvusClient) Healthy(ctx context.Context) bool {
	_, err := c.client.GetServerVersion(ctx, milvusclient.NewGetServerVersionOption())
	return err == nil
}

func (c *MilvusClient) ensureCollection(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("collection 名称不能为空")
	}
	c.collectionsMu.Lock()
	defer c.collectionsMu.Unlock()
	if _, ok := c.collections[name]; ok {
		return nil
	}
	has, err := c.client.HasCollection(ctx, milvusclient.NewHasCollectionOption(name))
	if err != nil {
		return fmt.Errorf("检查 collection %q 失败: %w", name, err)
	}
	if !has {
		schema := entity.NewSchema().
			WithDescription("AgentGo vector records").
			WithField(entity.NewField().WithName(idField).WithDataType(entity.FieldTypeVarChar).WithMaxLength(maxIDLength).WithIsPrimaryKey(true)).
			WithField(entity.NewField().WithName(contentField).WithDataType(entity.FieldTypeVarChar).WithMaxLength(maxContentLen)).
			WithField(entity.NewField().WithName(embeddingField).WithDataType(entity.FieldTypeFloatVector).WithDim(int64(c.dimension))).
			WithField(entity.NewField().WithName(metadataField).WithDataType(entity.FieldTypeJSON))
		if err := c.client.CreateCollection(ctx, milvusclient.NewCreateCollectionOption(name, schema).WithConsistencyLevel(entity.ClStrong)); err != nil {
			return fmt.Errorf("创建 collection %q 失败: %w", name, err)
		}
		task, err := c.client.CreateIndex(ctx, milvusclient.NewCreateIndexOption(name, embeddingField, index.NewAutoIndex(c.metricType)))
		if err != nil {
			return fmt.Errorf("创建 vector index 失败: %w", err)
		}
		if err := task.Await(ctx); err != nil {
			return fmt.Errorf("等待 vector index 失败: %w", err)
		}
	}
	loadTask, err := c.client.LoadCollection(ctx, milvusclient.NewLoadCollectionOption(name))
	if err != nil {
		return fmt.Errorf("加载 collection %q 失败: %w", name, err)
	}
	if err := loadTask.Await(ctx); err != nil {
		return fmt.Errorf("等待 collection %q 加载失败: %w", name, err)
	}
	c.collections[name] = struct{}{}
	return nil
}

func (c *MilvusClient) resolveCollection(name string) string {
	if name == "" {
		return c.collectionName
	}
	return name
}

func parseMetricType(raw string) (entity.MetricType, error) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "COSINE", "":
		return entity.COSINE, nil
	case "L2":
		return entity.L2, nil
	case "IP":
		return entity.IP, nil
	default:
		return "", fmt.Errorf("不支持的 Milvus metric_type %q，仅支持 COSINE、L2、IP", raw)
	}
}

func stringAt(col column.Column, index int) (string, error) {
	if col == nil {
		return "", fmt.Errorf("column 不存在")
	}
	value, err := col.Get(index)
	if err != nil {
		return "", err
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("column value 类型为 %T", value)
	}
	return text, nil
}

func metadataAt(col column.Column, index int) (map[string]string, error) {
	if col == nil {
		return nil, fmt.Errorf("column 不存在")
	}
	value, err := col.Get(index)
	if err != nil {
		return nil, err
	}
	var raw []byte
	switch value := value.(type) {
	case []byte:
		raw = value
	case string:
		raw = []byte(value)
	default:
		return nil, fmt.Errorf("metadata 类型为 %T", value)
	}
	metadata := make(map[string]string)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return nil, err
		}
	}
	return metadata, nil
}
