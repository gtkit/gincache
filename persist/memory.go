package persist

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// MemoryStore 内存缓存存储（生产级实现）
// 特点：
// - 使用 sync.Map 实现，读多写少场景性能好
// - 后台协程定期清理过期数据
// - 支持统计信息
type MemoryStore struct {
	data              sync.Map
	defaultExpiration time.Duration
	cleanupInterval   time.Duration
	stopCleanup       chan struct{}
	stopped           atomic.Bool

	// 统计
	hitCount  atomic.Uint64
	missCount atomic.Uint64
	setCount  atomic.Uint64
	delCount  atomic.Uint64
}

type memoryItem struct {
	value      []byte
	expiration int64 // Unix nano, 0 表示永不过期
}

// MemoryStoreOption 内存存储选项
type MemoryStoreOption func(*MemoryStore)

// WithCleanupInterval 设置清理间隔
func WithCleanupInterval(interval time.Duration) MemoryStoreOption {
	return func(s *MemoryStore) {
		s.cleanupInterval = interval
	}
}

// NewMemoryStore 创建内存存储
func NewMemoryStore(defaultExpiration time.Duration, opts ...MemoryStoreOption) *MemoryStore {
	s := &MemoryStore{
		defaultExpiration: defaultExpiration,
		cleanupInterval:   time.Minute, // 默认每分钟清理一次
		stopCleanup:       make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}

	// 启动后台清理协程
	go s.cleanupLoop()

	return s
}

// Get 从内存获取缓存
func (s *MemoryStore) Get(key string, value any) error {
	v, ok := s.data.Load(key)
	if !ok {
		s.missCount.Add(1)
		return ErrCacheMiss
	}

	item := v.(*memoryItem)

	// 检查是否过期
	if item.expiration > 0 && time.Now().UnixNano() > item.expiration {
		s.data.Delete(key)
		s.missCount.Add(1)
		return ErrCacheMiss
	}

	s.hitCount.Add(1)
	return json.Unmarshal(item.value, value)
}

// Set 设置内存缓存
func (s *MemoryStore) Set(key string, value any, expire time.Duration) error {
	return s.SetWithContext(context.Background(), key, value, expire)
}

// SetWithContext 带 Context 的设置缓存
func (s *MemoryStore) SetWithContext(_ context.Context, key string, value any, expire time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}

	if expire == 0 {
		expire = s.defaultExpiration
	}

	var expiration int64
	if expire > 0 {
		expiration = time.Now().Add(expire).UnixNano()
	}

	s.data.Store(key, &memoryItem{
		value:      data,
		expiration: expiration,
	})

	s.setCount.Add(1)
	return nil
}

// Delete 删除内存缓存
func (s *MemoryStore) Delete(key string) error {
	s.data.Delete(key)
	s.delCount.Add(1)
	return nil
}

// DeletePattern 按模式删除缓存（简单前缀匹配）
func (s *MemoryStore) DeletePattern(_ context.Context, pattern string) (int64, error) {
	// 简单实现：遍历所有 key，匹配前缀
	// 生产环境如果 key 很多，建议使用 Redis
	var deleted int64

	// 移除通配符
	prefix := pattern
	if len(prefix) > 0 && prefix[len(prefix)-1] == '*' {
		prefix = prefix[:len(prefix)-1]
	}

	s.data.Range(func(k, _ any) bool {
		key := k.(string)
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			s.data.Delete(key)
			deleted++
		}
		return true
	})

	s.delCount.Add(uint64(deleted))
	return deleted, nil
}

// Flush 清空所有缓存
func (s *MemoryStore) Flush() {
	s.data.Range(func(key, _ any) bool {
		s.data.Delete(key)
		return true
	})
}

// Close 关闭存储，停止清理协程
func (s *MemoryStore) Close() error {
	if s.stopped.CompareAndSwap(false, true) {
		close(s.stopCleanup)
	}
	return nil
}

// Stats 获取统计信息
func (s *MemoryStore) Stats() map[string]int64 {
	var count, expired int64
	now := time.Now().UnixNano()

	s.data.Range(func(_, value any) bool {
		count++
		item := value.(*memoryItem)
		if item.expiration > 0 && now > item.expiration {
			expired++
		}
		return true
	})

	hit := s.hitCount.Load()
	miss := s.missCount.Load()
	total := hit + miss

	var hitRate float64
	if total > 0 {
		hitRate = float64(hit) / float64(total) * 100
	}

	return map[string]int64{
		"keys":     count,
		"expired":  expired,
		"hit":      int64(hit),
		"miss":     int64(miss),
		"set":      int64(s.setCount.Load()),
		"del":      int64(s.delCount.Load()),
		"hit_rate": int64(hitRate), // 整数百分比
	}
}

// ResetStats 重置统计
func (s *MemoryStore) ResetStats() {
	s.hitCount.Store(0)
	s.missCount.Store(0)
	s.setCount.Store(0)
	s.delCount.Store(0)
}

// cleanupLoop 后台清理过期数据
func (s *MemoryStore) cleanupLoop() {
	ticker := time.NewTicker(s.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.deleteExpired()
		case <-s.stopCleanup:
			return
		}
	}
}

func (s *MemoryStore) deleteExpired() {
	now := time.Now().UnixNano()
	s.data.Range(func(key, value any) bool {
		item := value.(*memoryItem)
		if item.expiration > 0 && now > item.expiration {
			s.data.Delete(key)
		}
		return true
	})
}

// Len 返回当前缓存数量（包含已过期但未清理的）
func (s *MemoryStore) Len() int {
	var count int
	s.data.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}
