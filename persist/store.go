// Package persist 提供缓存存储后端接口和实现
package persist

import (
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
}
