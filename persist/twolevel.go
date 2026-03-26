package persist

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// TwoLevelStore 是一个面向生产的两级缓存实现。
// L1 是可插拔的本地缓存，默认使用 MemoryStore。
// L2 是 Redis，并作为共享缓存的事实来源。
type TwoLevelStore struct {
	local     LocalStore
	remote    *RedisStore
	localTTL  time.Duration
	remoteTTL time.Duration
	sf        singleflight.Group
	keyPrefix string

	localHit  atomic.Uint64
	remoteHit atomic.Uint64
	miss      atomic.Uint64
}

// TwoLevelStoreOption 用于配置 TwoLevelStore。
type TwoLevelStoreOption func(*TwoLevelStore)

// WithLocalTTL 设置本地缓存默认 TTL。
func WithLocalTTL(ttl time.Duration) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		s.localTTL = ttl
	}
}

// WithRemoteTTL 设置远端 Redis 默认 TTL。
func WithRemoteTTL(ttl time.Duration) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		s.remoteTTL = ttl
	}
}

// WithTwoLevelKeyPrefix 设置两级缓存使用的 Redis key 前缀。
func WithTwoLevelKeyPrefix(prefix string) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		s.keyPrefix = prefix
	}
}

// WithLocalStore 注入自定义本地缓存实现。
func WithLocalStore(local LocalStore) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		s.local = local
	}
}

// NewTwoLevelStore 创建一个 L1 可插拔、L2 为 Redis 的两级缓存。
func NewTwoLevelStore(redisClient redis.Cmdable, opts ...TwoLevelStoreOption) *TwoLevelStore {
	s := &TwoLevelStore{
		localTTL:  30 * time.Second,
		remoteTTL: 5 * time.Minute,
		keyPrefix: "gincache:2l:",
	}

	for _, opt := range opts {
		opt(s)
	}

	if s.local == nil {
		cleanupInterval := s.localTTL / 2
		if cleanupInterval < 10*time.Second {
			cleanupInterval = 10 * time.Second
		}
		s.local = NewMemoryStore(s.localTTL, WithCleanupInterval(cleanupInterval))
	}

	s.remote = NewRedisStore(redisClient, WithKeyPrefix(s.keyPrefix))
	return s
}

func (s *TwoLevelStore) key(k string) string {
	return s.keyPrefix + k
}

// Get 优先读取本地缓存，未命中时回源 Redis，并在远端命中后回填本地。
func (s *TwoLevelStore) Get(key string, value any) error {
	if err := s.local.Get(key, value); err == nil {
		s.localHit.Add(1)
		return nil
	}

	result, err, _ := s.sf.Do(key, func() (any, error) {
		var temp json.RawMessage
		if err := s.local.Get(key, &temp); err == nil {
			return temp, nil
		}

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		data, err := s.remote.Client().Get(ctx, s.remote.key(key)).Bytes()
		if err == redis.Nil {
			return nil, ErrCacheMiss
		}
		if err != nil {
			return nil, err
		}

		var cached any
		if json.Unmarshal(data, &cached) == nil {
			_ = s.local.Set(key, cached, s.localTTL)
		}

		return data, nil
	})
	if err != nil {
		s.miss.Add(1)
		return err
	}

	s.remoteHit.Add(1)

	switch v := result.(type) {
	case []byte:
		return json.Unmarshal(v, value)
	case json.RawMessage:
		return json.Unmarshal(v, value)
	default:
		data, _ := json.Marshal(v)
		return json.Unmarshal(data, value)
	}
}

// Set 先写 Redis，成功后再写本地缓存。
func (s *TwoLevelStore) Set(key string, value any, expire time.Duration) error {
	return s.SetWithContext(context.Background(), key, value, expire)
}

// SetWithContext 先写 Redis，再写本地缓存，避免远端失败导致本地脏数据。
func (s *TwoLevelStore) SetWithContext(ctx context.Context, key string, value any, expire time.Duration) error {
	localExpire := s.localTTL
	remoteExpire := expire
	if remoteExpire == 0 {
		remoteExpire = s.remoteTTL
	}

	if localExpire > remoteExpire {
		localExpire = remoteExpire
	}

	if err := s.remote.SetWithContext(ctx, key, value, remoteExpire); err != nil {
		return err
	}

	return s.local.Set(key, value, localExpire)
}

// Delete 同时删除本地缓存和 Redis 中的单个 key。
func (s *TwoLevelStore) Delete(key string) error {
	_ = s.local.Delete(key)
	return s.remote.Delete(key)
}

// DeletePattern 先尽力删除本地缓存，再删除 Redis 中匹配模式的 key。
func (s *TwoLevelStore) DeletePattern(ctx context.Context, pattern string) (int64, error) {
	if local, ok := s.local.(LocalStoreWithPattern); ok {
		_, _ = local.DeletePattern(ctx, pattern)
	}
	return s.remote.DeletePattern(ctx, pattern)
}

// InvalidateLocal 只删除本地缓存中的单个 key。
func (s *TwoLevelStore) InvalidateLocal(key string) {
	_ = s.local.Delete(key)
}

// InvalidateLocalPattern 在本地缓存支持按模式删除时，清理本地匹配 key。
func (s *TwoLevelStore) InvalidateLocalPattern(pattern string) {
	if local, ok := s.local.(LocalStoreWithPattern); ok {
		_, _ = local.DeletePattern(context.Background(), pattern)
	}
}

// Stats 返回两级缓存的聚合统计信息。
func (s *TwoLevelStore) Stats() map[string]int64 {
	localStats := s.local.Stats()

	localHit := s.localHit.Load()
	remoteHit := s.remoteHit.Load()
	miss := s.miss.Load()
	total := localHit + remoteHit + miss

	var hitRate float64
	if total > 0 {
		hitRate = float64(localHit+remoteHit) / float64(total) * 100
	}

	var localHitRate float64
	if localHit+remoteHit > 0 {
		localHitRate = float64(localHit) / float64(localHit+remoteHit) * 100
	}

	return map[string]int64{
		"local_keys":     localStats["keys"],
		"local_hit":      int64(localHit),
		"remote_hit":     int64(remoteHit),
		"miss":           int64(miss),
		"total_hit_rate": int64(hitRate),
		"local_hit_rate": int64(localHitRate),
	}
}

// ResetStats 重置两级缓存的聚合计数和本地缓存计数。
func (s *TwoLevelStore) ResetStats() {
	s.localHit.Store(0)
	s.remoteHit.Store(0)
	s.miss.Store(0)
	s.local.ResetStats()
}

// Close 关闭本地缓存。
func (s *TwoLevelStore) Close() error {
	return s.local.Close()
}

// LocalStore 返回当前配置的本地缓存实现。
func (s *TwoLevelStore) LocalStore() LocalStore {
	return s.local
}

// RemoteStore 返回 Redis 实现的二级缓存。
func (s *TwoLevelStore) RemoteStore() *RedisStore {
	return s.remote
}
