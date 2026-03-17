package persist

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore Redis 缓存存储
type RedisStore struct {
	client    redis.Cmdable
	keyPrefix string
}

// RedisStoreOption Redis 存储选项
type RedisStoreOption func(*RedisStore)

// WithKeyPrefix 设置 Key 前缀
func WithKeyPrefix(prefix string) RedisStoreOption {
	return func(s *RedisStore) {
		s.keyPrefix = prefix
	}
}

// NewRedisStore 创建 Redis 存储
// 支持 *redis.Client, *redis.ClusterClient, *redis.Ring
func NewRedisStore(client redis.Cmdable, opts ...RedisStoreOption) *RedisStore {
	s := &RedisStore{
		client:    client,
		keyPrefix: "gincache:",
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *RedisStore) key(k string) string {
	return s.keyPrefix + k
}

// Get 从 Redis 获取缓存
func (s *RedisStore) Get(key string, value any) error {
	ctx := context.Background()
	data, err := s.client.Get(ctx, s.key(key)).Bytes()
	if err == redis.Nil {
		return ErrCacheMiss
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

// Set 设置 Redis 缓存
func (s *RedisStore) Set(key string, value any, expire time.Duration) error {
	ctx := context.Background()
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, s.key(key), data, expire).Err()
}

// Delete 删除 Redis 缓存
func (s *RedisStore) Delete(key string) error {
	ctx := context.Background()
	return s.client.Del(ctx, s.key(key)).Err()
}

// DeletePattern 按模式删除缓存
// 注意：生产环境慎用 KEYS 命令，建议使用 SCAN
func (s *RedisStore) DeletePattern(pattern string) error {
	ctx := context.Background()

	var cursor uint64
	for {
		keys, nextCursor, err := s.client.Scan(ctx, cursor, s.key(pattern), 100).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := s.client.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return nil
}

// Client 获取底层 Redis 客户端
func (s *RedisStore) Client() redis.Cmdable {
	return s.client
}
