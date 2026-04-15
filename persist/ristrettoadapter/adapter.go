package ristrettoadapter

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	ristretto "github.com/dgraph-io/ristretto/v2"
	"github.com/gtkit/gincache/persist"
	cachepattern "github.com/gtkit/gincache/persist/internal/pattern"
)

// CostFunc 用于把序列化后的值转换成 Ristretto 的成本值。
type CostFunc func(key string, value []byte) int64

// Store 是基于外部注入的 Ristretto 实例封装出的本地缓存实现。
type Store struct {
	cache             *ristretto.Cache[string, []byte]
	defaultExpiration time.Duration
	costFn            CostFunc
	waitOnMutation    bool
	closeUnderlying   bool

	keys sync.Map

	keyCount    atomic.Int64
	hitCount    atomic.Uint64
	missCount   atomic.Uint64
	setCount    atomic.Uint64
	delCount    atomic.Uint64
	rejectCount atomic.Uint64
}

// Option 用于配置 Store 包装层行为。
type Option func(*Store)

// WithDefaultExpiration 设置 Set 在传入零过期时间时使用的默认 TTL。
func WithDefaultExpiration(ttl time.Duration) Option {
	return func(s *Store) {
		s.defaultExpiration = ttl
	}
}

// WithCost 设置写入时使用的成本计算函数。
func WithCost(fn CostFunc) Option {
	return func(s *Store) {
		s.costFn = fn
	}
}

// WithWait 让写入和删除在返回前等待 Ristretto 的异步缓冲刷新完成。
func WithWait() Option {
	return func(s *Store) {
		s.waitOnMutation = true
	}
}

// WithOwnedCache 表示在调用 Close 时同时关闭底层 Ristretto 实例。
func WithOwnedCache() Option {
	return func(s *Store) {
		s.closeUnderlying = true
	}
}

// New 把一个现有的 Ristretto 实例包装为 persist.LocalStore。
func New(cache *ristretto.Cache[string, []byte], opts ...Option) *Store {
	store := &Store{
		cache:  cache,
		costFn: func(_ string, value []byte) int64 { return int64(len(value)) },
	}
	for _, opt := range opts {
		opt(store)
	}
	return store
}

// WithLocalStore 直接把现有 Ristretto 实例转换成 TwoLevelStore 的本地缓存选项。
func WithLocalStore(cache *ristretto.Cache[string, []byte], opts ...Option) persist.TwoLevelStoreOption {
	return persist.WithLocalStore(New(cache, opts...))
}

// Cache 返回当前包装的底层 Ristretto 实例。
func (s *Store) Cache() *ristretto.Cache[string, []byte] {
	return s.cache
}

// Get 从 Ristretto 中读取并反序列化值。
func (s *Store) Get(key string, value any) error {
	data, ok := s.cache.Get(key)
	if !ok {
		s.missCount.Add(1)
		return persist.ErrCacheMiss
	}

	s.hitCount.Add(1)
	return json.Unmarshal(data, value)
}

// Set 写入 Ristretto。
func (s *Store) Set(key string, value any, expire time.Duration) error {
	return s.SetWithContext(context.Background(), key, value, expire)
}

// SetWithContext 写入 Ristretto，Context 当前只用于保持与主接口一致。
func (s *Store) SetWithContext(_ context.Context, key string, value any, expire time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}

	if expire == 0 {
		expire = s.defaultExpiration
	}

	cost := s.costFn(key, data)
	var accepted bool
	if expire > 0 {
		accepted = s.cache.SetWithTTL(key, data, cost, expire)
	} else {
		accepted = s.cache.Set(key, data, cost)
	}

	s.setCount.Add(1)
	if !accepted {
		s.rejectCount.Add(1)
		return nil
	}

	s.trackKey(key)
	s.wait()
	return nil
}

// Delete 删除单个 key。
func (s *Store) Delete(key string) error {
	s.cache.Del(key)
	s.untrackKey(key)
	s.delCount.Add(1)
	s.wait()
	return nil
}

// DeletePattern 按模式删除 key。
func (s *Store) DeletePattern(_ context.Context, pattern string) (int64, error) {
	matcher, err := cachepattern.Compile(pattern)
	if err != nil {
		return 0, err
	}

	var deleted int64

	s.keys.Range(func(k, _ any) bool {
		key := k.(string)
		if matcher.Match(key) {
			s.cache.Del(key)
			s.untrackKey(key)
			deleted++
		}
		return true
	})

	s.delCount.Add(uint64(deleted))
	s.wait()
	return deleted, nil
}

// Close 关闭包装层，必要时关闭底层 Ristretto 实例。
func (s *Store) Close() error {
	if s.closeUnderlying && s.cache != nil {
		s.cache.Close()
	}
	return nil
}

// Stats 返回包装层维护的统计信息。
func (s *Store) Stats() map[string]int64 {
	hit := s.hitCount.Load()
	miss := s.missCount.Load()
	total := hit + miss

	var hitRate float64
	if total > 0 {
		hitRate = float64(hit) / float64(total) * 100
	}

	keys := s.keyCount.Load()
	return map[string]int64{
		"keys":      keys,
		"tracked":   keys,
		"hit":       int64(hit),
		"miss":      int64(miss),
		"set":       int64(s.setCount.Load()),
		"del":       int64(s.delCount.Load()),
		"reject":    int64(s.rejectCount.Load()),
		"hit_rate":  int64(hitRate),
		"max_cost":  s.cache.MaxCost(),
		"remaining": s.cache.RemainingCost(),
	}
}

// ResetStats 重置包装层统计计数。
func (s *Store) ResetStats() {
	s.hitCount.Store(0)
	s.missCount.Store(0)
	s.setCount.Store(0)
	s.delCount.Store(0)
	s.rejectCount.Store(0)
}

func (s *Store) trackKey(key string) {
	if _, loaded := s.keys.LoadOrStore(key, struct{}{}); !loaded {
		s.keyCount.Add(1)
	}
}

func (s *Store) untrackKey(key string) {
	if _, loaded := s.keys.LoadAndDelete(key); loaded {
		s.keyCount.Add(-1)
	}
}

func (s *Store) wait() {
	if s.waitOnMutation {
		s.cache.Wait()
	}
}
