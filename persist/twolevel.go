package persist

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// TwoLevelStore 两级缓存存储
// L1: 本地内存（热点数据，低延迟）
// L2: Redis（容量大，跨实例共享）
type TwoLevelStore struct {
	local       *MemoryStore
	remote      *RedisStore
	localTTL    time.Duration // 本地缓存 TTL（通常比 Redis 短）
	sf          singleflight.Group
	keyPrefix   string
	syncOnMiss  bool // 未命中时是否同步回填本地缓存
}

// TwoLevelStoreOption 两级缓存选项
type TwoLevelStoreOption func(*TwoLevelStore)

// WithLocalTTL 设置本地缓存 TTL
func WithLocalTTL(ttl time.Duration) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		s.localTTL = ttl
	}
}

// WithTwoLevelKeyPrefix 设置 Key 前缀
func WithTwoLevelKeyPrefix(prefix string) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		s.keyPrefix = prefix
	}
}

// WithSyncOnMiss 设置未命中时是否同步回填
func WithSyncOnMiss(sync bool) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		s.syncOnMiss = sync
	}
}

// NewTwoLevelStore 创建两级缓存存储
func NewTwoLevelStore(
	localExpiration time.Duration,
	redisClient redis.Cmdable,
	opts ...TwoLevelStoreOption,
) *TwoLevelStore {
	s := &TwoLevelStore{
		local:      NewMemoryStore(localExpiration),
		remote:     NewRedisStore(redisClient),
		localTTL:   localExpiration,
		keyPrefix:  "gincache:2l:",
		syncOnMiss: true,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *TwoLevelStore) key(k string) string {
	return s.keyPrefix + k
}

// Get 获取缓存：本地 -> Redis
func (s *TwoLevelStore) Get(key string, value any) error {
	fullKey := s.key(key)

	// 1. 先查本地缓存
	if err := s.local.Get(fullKey, value); err == nil {
		return nil
	}

	// 2. 查 Redis（使用 singleflight 防止并发穿透）
	result, err, _ := s.sf.Do(fullKey, func() (any, error) {
		// Double-check 本地缓存
		var temp any
		if err := s.local.Get(fullKey, &temp); err == nil {
			return temp, nil
		}

		// 查 Redis
		ctx := context.Background()
		data, err := s.remote.Client().Get(ctx, fullKey).Bytes()
		if err == redis.Nil {
			return nil, ErrCacheMiss
		}
		if err != nil {
			return nil, err
		}

		// 回填本地缓存
		if s.syncOnMiss {
			var cached any
			if json.Unmarshal(data, &cached) == nil {
				s.local.Set(fullKey, cached, s.localTTL)
			}
		}

		return data, nil
	})

	if err != nil {
		return err
	}

	// 反序列化
	switch v := result.(type) {
	case []byte:
		return json.Unmarshal(v, value)
	default:
		// 已经反序列化过了
		data, _ := json.Marshal(v)
		return json.Unmarshal(data, value)
	}
}

// Set 设置缓存：同时写入本地和 Redis
func (s *TwoLevelStore) Set(key string, value any, expire time.Duration) error {
	fullKey := s.key(key)

	// 写入本地缓存（使用较短的 TTL）
	localExpire := s.localTTL
	if expire > 0 && expire < localExpire {
		localExpire = expire
	}
	if err := s.local.Set(fullKey, value, localExpire); err != nil {
		return err
	}

	// 写入 Redis
	return s.remote.Set(fullKey, value, expire)
}

// Delete 删除缓存：同时删除本地和 Redis
func (s *TwoLevelStore) Delete(key string) error {
	fullKey := s.key(key)

	// 删除本地
	s.local.Delete(fullKey)

	// 删除 Redis
	return s.remote.Delete(fullKey)
}

// InvalidateLocal 仅失效本地缓存
// 用于收到 Redis Pub/Sub 消息时清理本地缓存
func (s *TwoLevelStore) InvalidateLocal(key string) {
	s.local.Delete(s.key(key))
}

// LocalStats 获取本地缓存统计
func (s *TwoLevelStore) LocalStats() map[string]int64 {
	return s.local.Stats()
}

// Close 关闭存储
func (s *TwoLevelStore) Close() {
	s.local.Close()
}
