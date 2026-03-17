package persist

import (
	"encoding/json"
	"sync"
	"time"
)

// MemoryStore 内存缓存存储.
// 使用 sync.Map 实现，适合读多写少场景
type MemoryStore struct {
	data              sync.Map
	defaultExpiration time.Duration
	cleanupInterval   time.Duration
	stopCleanup       chan struct{}
}

type memoryItem struct {
	value      []byte
	expiration int64 // Unix nano
}

// MemoryStoreOption 内存存储选项.
type MemoryStoreOption func(*MemoryStore)

// WithCleanupInterval 设置清理间隔
func WithCleanupInterval(interval time.Duration) MemoryStoreOption {
	return func(s *MemoryStore) {
		s.cleanupInterval = interval
	}
}

// NewMemoryStore 创建内存存储.
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

// Get 从内存获取缓存.
func (s *MemoryStore) Get(key string, value any) error {
	v, ok := s.data.Load(key)
	if !ok {
		return ErrCacheMiss
	}

	item := v.(*memoryItem)

	// 检查是否过期
	if item.expiration > 0 && time.Now().UnixNano() > item.expiration {
		s.data.Delete(key)
		return ErrCacheMiss
	}

	return json.Unmarshal(item.value, value)
}

// Set 设置内存缓存.
func (s *MemoryStore) Set(key string, value any, expire time.Duration) error {
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

	return nil
}

// Delete 删除内存缓存.
func (s *MemoryStore) Delete(key string) error {
	s.data.Delete(key)
	return nil
}

// Flush 清空所有缓存.
func (s *MemoryStore) Flush() {
	s.data.Range(func(key, _ any) bool {
		s.data.Delete(key)
		return true
	})
}

// Close 关闭存储，停止清理协程.
func (s *MemoryStore) Close() {
	close(s.stopCleanup)
}

// cleanupLoop 后台清理过期数据.
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

// Stats 获取统计信息.
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

	return map[string]int64{
		"count":   count,
		"expired": expired,
	}
}
