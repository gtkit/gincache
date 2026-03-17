// Package persist 提供缓存存储后端接口和实现
package persist

import (
	"context"
	"errors"
	"time"
)

// ErrCacheMiss 缓存未命中错误
var ErrCacheMiss = errors.New("cache: key not found")

// CacheStore 缓存存储接口
type CacheStore interface {
	// Get 获取缓存，如果不存在返回 ErrCacheMiss
	Get(key string, value any) error

	// Set 设置缓存
	Set(key string, value any, expire time.Duration) error

	// Delete 删除缓存
	Delete(key string) error

	// SetWithContext 带 Context 的设置缓存（支持超时控制）
	SetWithContext(ctx context.Context, key string, value any, expire time.Duration) error
}

// CacheStoreWithPattern 支持模式删除的缓存存储接口
type CacheStoreWithPattern interface {
	CacheStore
	// DeletePattern 按模式删除缓存
	DeletePattern(ctx context.Context, pattern string) (int64, error)
}

// CacheStoreWithStats 支持统计的缓存存储接口
type CacheStoreWithStats interface {
	CacheStore
	// Stats 获取统计信息
	Stats() map[string]int64
}
