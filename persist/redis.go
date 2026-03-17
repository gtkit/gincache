package persist

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore Redis 缓存存储（生产级实现）
type RedisStore struct {
	client       redis.Cmdable
	keyPrefix    string
	defaultCtx   context.Context
	readTimeout  time.Duration
	writeTimeout time.Duration
}

// RedisStoreOption Redis 存储选项
type RedisStoreOption func(*RedisStore)

// WithKeyPrefix 设置 Key 前缀
func WithKeyPrefix(prefix string) RedisStoreOption {
	return func(s *RedisStore) {
		s.keyPrefix = prefix
	}
}

// WithReadTimeout 设置读取超时
func WithReadTimeout(timeout time.Duration) RedisStoreOption {
	return func(s *RedisStore) {
		s.readTimeout = timeout
	}
}

// WithWriteTimeout 设置写入超时
func WithWriteTimeout(timeout time.Duration) RedisStoreOption {
	return func(s *RedisStore) {
		s.writeTimeout = timeout
	}
}

// NewRedisStore 创建 Redis 存储
// 支持 *redis.Client, *redis.ClusterClient, *redis.Ring
func NewRedisStore(client redis.Cmdable, opts ...RedisStoreOption) *RedisStore {
	s := &RedisStore{
		client:       client,
		keyPrefix:    "gincache:",
		defaultCtx:   context.Background(),
		readTimeout:  3 * time.Second,
		writeTimeout: 3 * time.Second,
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
	ctx, cancel := context.WithTimeout(s.defaultCtx, s.readTimeout)
	defer cancel()

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
	ctx, cancel := context.WithTimeout(s.defaultCtx, s.writeTimeout)
	defer cancel()
	return s.SetWithContext(ctx, key, value, expire)
}

// SetWithContext 带 Context 的设置缓存
func (s *RedisStore) SetWithContext(ctx context.Context, key string, value any, expire time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, s.key(key), data, expire).Err()
}

// Delete 删除 Redis 缓存
func (s *RedisStore) Delete(key string) error {
	ctx, cancel := context.WithTimeout(s.defaultCtx, s.writeTimeout)
	defer cancel()
	return s.client.Del(ctx, s.key(key)).Err()
}

// DeleteWithContext 带 Context 删除缓存
func (s *RedisStore) DeleteWithContext(ctx context.Context, key string) error {
	return s.client.Del(ctx, s.key(key)).Err()
}

// DeletePattern 按模式删除缓存（使用 SCAN，生产安全）
func (s *RedisStore) DeletePattern(ctx context.Context, pattern string) (int64, error) {
	fullPattern := s.key(pattern)
	var cursor uint64
	var deleted int64

	for {
		// 使用 SCAN 避免阻塞
		keys, nextCursor, err := s.client.Scan(ctx, cursor, fullPattern, 100).Result()
		if err != nil {
			return deleted, err
		}

		if len(keys) > 0 {
			// 批量删除
			n, err := s.client.Del(ctx, keys...).Result()
			if err != nil {
				return deleted, err
			}
			deleted += n
		}

		cursor = nextCursor
		if cursor == 0 {
			break
		}

		// 检查 Context 是否取消
		select {
		case <-ctx.Done():
			return deleted, ctx.Err()
		default:
		}
	}

	return deleted, nil
}

// DeleteKeys 批量删除指定的 keys
func (s *RedisStore) DeleteKeys(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}

	fullKeys := make([]string, len(keys))
	for i, k := range keys {
		fullKeys[i] = s.key(k)
	}

	return s.client.Del(ctx, fullKeys...).Err()
}

// Exists 检查 key 是否存在
func (s *RedisStore) Exists(ctx context.Context, key string) (bool, error) {
	n, err := s.client.Exists(ctx, s.key(key)).Result()
	return n > 0, err
}

// TTL 获取 key 的剩余过期时间
func (s *RedisStore) TTL(ctx context.Context, key string) (time.Duration, error) {
	return s.client.TTL(ctx, s.key(key)).Result()
}

// Client 获取底层 Redis 客户端
func (s *RedisStore) Client() redis.Cmdable {
	return s.client
}

// Ping 检查 Redis 连接
func (s *RedisStore) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}

// Stats 获取统计信息
func (s *RedisStore) Stats() map[string]int64 {
	ctx, cancel := context.WithTimeout(s.defaultCtx, s.readTimeout)
	defer cancel()

	// 统计当前前缀下的 key 数量
	var count int64
	var cursor uint64
	for {
		keys, nextCursor, err := s.client.Scan(ctx, cursor, s.key("*"), 1000).Result()
		if err != nil {
			break
		}
		count += int64(len(keys))
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	return map[string]int64{
		"keys": count,
	}
}
