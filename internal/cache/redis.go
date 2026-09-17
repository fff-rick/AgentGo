// Package cache 提供缓存层的抽象和 Redis 实现。
package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/enterprise/ai-agent-go/internal/config"
	"github.com/enterprise/ai-agent-go/internal/metrics"
	"github.com/enterprise/ai-agent-go/internal/trace"
)

// Cache 缓存操作接口
type Cache interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key string, value string, ttl time.Duration) error
	CompareAndSet(ctx context.Context, key, expected, value string, ttl time.Duration) (bool, error)
	Delete(ctx context.Context, key string) error
	Exists(ctx context.Context, key string) (bool, error)
	LPush(ctx context.Context, key string, values ...interface{}) error
	LRange(ctx context.Context, key string, start, stop int64) ([]string, error)
	LTrim(ctx context.Context, key string, start, stop int64) error
	Expire(ctx context.Context, key string, ttl time.Duration) error
	Incr(ctx context.Context, key string) (int64, error)
	Close() error
	Healthy(ctx context.Context) bool
}

// RedisCache 基于 Redis 的缓存实现
type RedisCache struct {
	client *redis.Client
}

var compareAndSetScript = redis.NewScript(`
if (redis.call('GET', KEYS[1]) or '') ~= ARGV[1] then return 0 end
redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
return 1`)

// NewRedisCache 创建 Redis 缓存实例并测试连接
func NewRedisCache(cfg config.RedisConfig) (*RedisCache, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		PoolSize:     cfg.PoolSize,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	})
	client.AddHook(redisMetricsHook{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("Redis 连接失败: %w", err)
	}

	return &RedisCache{client: client}, nil
}

type redisMetricsHook struct{}
type redisStartKey struct{}

func (redisMetricsHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	ctx, _ = trace.StartSpan(ctx, "redis."+redisOperation(cmd.Name()))
	return context.WithValue(ctx, redisStartKey{}, time.Now()), nil
}
func (redisMetricsHook) AfterProcess(ctx context.Context, cmd redis.Cmder) error {
	if start, ok := ctx.Value(redisStartKey{}).(time.Time); ok {
		op := redisOperation(cmd.Name())
		metrics.Default.DependencyDuration.WithLabelValues("redis", op).Observe(metrics.Seconds(start))
	}
	err := cmd.Err()
	if err == redis.Nil {
		err = nil
	}
	trace.Finish(oteltrace.SpanFromContext(ctx), err)
	return nil
}
func (redisMetricsHook) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	ctx, _ = trace.StartSpan(ctx, "redis.pipeline")
	return context.WithValue(ctx, redisStartKey{}, time.Now()), nil
}
func (redisMetricsHook) AfterProcessPipeline(ctx context.Context, _ []redis.Cmder) error {
	if start, ok := ctx.Value(redisStartKey{}).(time.Time); ok {
		metrics.Default.DependencyDuration.WithLabelValues("redis", "pipeline").Observe(metrics.Seconds(start))
	}
	oteltrace.SpanFromContext(ctx).End()
	return nil
}

func redisOperation(name string) string {
	switch name {
	case "get", "set", "del", "exists", "lpush", "lrange", "ltrim", "expire", "incr", "eval", "evalsha", "ping":
		return name
	default:
		return "other"
	}
}

// Get 获取缓存值
func (r *RedisCache) Get(ctx context.Context, key string) (string, error) {
	val, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return val, err
}

// Set 设置缓存值，ttl 为 0 表示永不过期
func (r *RedisCache) Set(ctx context.Context, key string, value string, ttl time.Duration) error {
	return r.client.Set(ctx, key, value, ttl).Err()
}

func (r *RedisCache) CompareAndSet(ctx context.Context, key, expected, value string, ttl time.Duration) (bool, error) {
	result, err := compareAndSetScript.Run(ctx, r.client, []string{key}, expected, value, ttl.Milliseconds()).Int()
	return result == 1, err
}

// Delete 删除缓存
func (r *RedisCache) Delete(ctx context.Context, key string) error {
	return r.client.Del(ctx, key).Err()
}

// Exists 判断 key 是否存在
func (r *RedisCache) Exists(ctx context.Context, key string) (bool, error) {
	n, err := r.client.Exists(ctx, key).Result()
	return n > 0, err
}

// LPush 从左侧推入列表
func (r *RedisCache) LPush(ctx context.Context, key string, values ...interface{}) error {
	return r.client.LPush(ctx, key, values...).Err()
}

// LRange 获取列表指定范围的元素
func (r *RedisCache) LRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	return r.client.LRange(ctx, key, start, stop).Result()
}

// LTrim 裁剪列表，只保留指定范围
func (r *RedisCache) LTrim(ctx context.Context, key string, start, stop int64) error {
	return r.client.LTrim(ctx, key, start, stop).Err()
}

func (r *RedisCache) Expire(ctx context.Context, key string, ttl time.Duration) error {
	return r.client.Expire(ctx, key, ttl).Err()
}

func (r *RedisCache) Incr(ctx context.Context, key string) (int64, error) {
	return r.client.Incr(ctx, key).Result()
}

// Close 关闭 Redis 连接
func (r *RedisCache) Close() error {
	return r.client.Close()
}

// Healthy 检查 Redis 是否可用
func (r *RedisCache) Healthy(ctx context.Context) bool {
	return r.client.Ping(ctx).Err() == nil
}

// GetJSON 获取缓存并反序列化为指定类型
func (r *RedisCache) GetJSON(ctx context.Context, key string, dest interface{}) error {
	val, err := r.Get(ctx, key)
	if err != nil {
		return err
	}
	if val == "" {
		return redis.Nil
	}
	return json.Unmarshal([]byte(val), dest)
}

// SetJSON 将对象序列化后写入缓存
func (r *RedisCache) SetJSON(ctx context.Context, key string, value interface{}, ttl time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("JSON 序列化失败: %w", err)
	}
	return r.Set(ctx, key, string(data), ttl)
}
