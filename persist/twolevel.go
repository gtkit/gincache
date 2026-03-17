package persist

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// TwoLevelStore 两级缓存存储（生产级实现）
// L1: 本地内存（热点数据，低延迟，进程内有效）
// L2: Redis（容量大，跨实例共享）
//
// 特点：
// - 读取时先查本地，再查 Redis
// - 写入时同时写本地和 Redis
// - singleflight 防止缓存击穿
// - 本地缓存 TTL 比 Redis 短，保证数据一致性
type TwoLevelStore struct {
	local     *MemoryStore
	remote    *RedisStore
	localTTL  time.Duration
	remoteTTL time.Duration
	sf        singleflight.Group
	keyPrefix string

	// 统计
	localHit  atomic.Uint64
	remoteHit atomic.Uint64
	miss      atomic.Uint64
}

// TwoLevelStoreOption 两级缓存选项
type TwoLevelStoreOption func(*TwoLevelStore)

// WithLocalTTL 设置本地缓存 TTL
func WithLocalTTL(ttl time.Duration) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		s.localTTL = ttl
	}
}

// WithRemoteTTL 设置远程缓存 TTL
func WithRemoteTTL(ttl time.Duration) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		s.remoteTTL = ttl
	}
}

// WithTwoLevelKeyPrefix 设置 Key 前缀
func WithTwoLevelKeyPrefix(prefix string) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		s.keyPrefix = prefix
	}
}

// NewTwoLevelStore 创建两级缓存存储
func NewTwoLevelStore(redisClient redis.Cmdable, opts ...TwoLevelStoreOption) *TwoLevelStore {
	s := &TwoLevelStore{
		localTTL:  30 * time.Second, // 默认本地 30 秒
		remoteTTL: 5 * time.Minute,  // 默认远程 5 分钟
		keyPrefix: "gincache:2l:",
	}

	for _, opt := range opts {
		opt(s)
	}

	// 创建本地存储（清理间隔为 TTL 的一半）
	cleanupInterval := s.localTTL / 2
	if cleanupInterval < 10*time.Second {
		cleanupInterval = 10 * time.Second
	}
	s.local = NewMemoryStore(s.localTTL, WithCleanupInterval(cleanupInterval))

	// 创建远程存储
	s.remote = NewRedisStore(redisClient, WithKeyPrefix(s.keyPrefix))

	return s
}

func (s *TwoLevelStore) key(k string) string {
	return s.keyPrefix + k
}

// Get 获取缓存：本地 -> Redis
func (s *TwoLevelStore) Get(key string, value any) error {
	// 1. 先查本地缓存
	if err := s.local.Get(key, value); err == nil {
		s.localHit.Add(1)
		return nil
	}

	// 2. 查 Redis（使用 singleflight 防止并发穿透）
	result, err, _ := s.sf.Do(key, func() (any, error) {
		// Double-check 本地缓存
		var temp json.RawMessage
		if err := s.local.Get(key, &temp); err == nil {
			return temp, nil
		}

		// 查 Redis
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		data, err := s.remote.Client().Get(ctx, s.remote.key(key)).Bytes()
		if err == redis.Nil {
			return nil, ErrCacheMiss
		}
		if err != nil {
			return nil, err
		}

		// 回填本地缓存
		var cached any
		if json.Unmarshal(data, &cached) == nil {
			s.local.Set(key, cached, s.localTTL)
		}

		return data, nil
	})

	if err != nil {
		s.miss.Add(1)
		return err
	}

	s.remoteHit.Add(1)

	// 反序列化
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

// Set 设置缓存：同时写入本地和 Redis
func (s *TwoLevelStore) Set(key string, value any, expire time.Duration) error {
	return s.SetWithContext(context.Background(), key, value, expire)
}

// SetWithContext 带 Context 的设置缓存
func (s *TwoLevelStore) SetWithContext(ctx context.Context, key string, value any, expire time.Duration) error {
	// 确定 TTL
	localExpire := s.localTTL
	remoteExpire := expire
	if remoteExpire == 0 {
		remoteExpire = s.remoteTTL
	}

	// 本地 TTL 不应超过远程 TTL
	if localExpire > remoteExpire {
		localExpire = remoteExpire
	}

	// 写入本地缓存
	if err := s.local.Set(key, value, localExpire); err != nil {
		return err
	}

	// 写入 Redis
	return s.remote.SetWithContext(ctx, key, value, remoteExpire)
}

// Delete 删除缓存：同时删除本地和 Redis
func (s *TwoLevelStore) Delete(key string) error {
	// 删除本地
	s.local.Delete(key)

	// 删除 Redis
	return s.remote.Delete(key)
}

// DeletePattern 按模式删除缓存
func (s *TwoLevelStore) DeletePattern(ctx context.Context, pattern string) (int64, error) {
	// 删除本地
	s.local.DeletePattern(ctx, pattern)

	// 删除 Redis
	return s.remote.DeletePattern(ctx, pattern)
}

// InvalidateLocal 仅失效本地缓存
// 用于收到 Redis Pub/Sub 消息时清理本地缓存
func (s *TwoLevelStore) InvalidateLocal(key string) {
	s.local.Delete(key)
}

// InvalidateLocalPattern 按模式失效本地缓存
func (s *TwoLevelStore) InvalidateLocalPattern(pattern string) {
	s.local.DeletePattern(context.Background(), pattern)
}

// Stats 获取统计信息
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

// ResetStats 重置统计
func (s *TwoLevelStore) ResetStats() {
	s.localHit.Store(0)
	s.remoteHit.Store(0)
	s.miss.Store(0)
	s.local.ResetStats()
}

// Close 关闭存储
func (s *TwoLevelStore) Close() error {
	return s.local.Close()
}

// LocalStore 获取本地存储（用于调试）
func (s *TwoLevelStore) LocalStore() *MemoryStore {
	return s.local
}

// RemoteStore 获取远程存储（用于调试）
func (s *TwoLevelStore) RemoteStore() *RedisStore {
	return s.remote
}
