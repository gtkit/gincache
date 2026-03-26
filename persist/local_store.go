package persist

import (
	"context"
	"time"
)

// LocalStore 定义了 TwoLevelStore 使用的可插拔本地缓存接口。
type LocalStore interface {
	Get(key string, value any) error
	Set(key string, value any, expire time.Duration) error
	Delete(key string) error
	Close() error
	Stats() map[string]int64
	ResetStats()
}

// LocalStoreWithPattern 表示本地缓存支持按模式删除。
type LocalStoreWithPattern interface {
	LocalStore
	DeletePattern(ctx context.Context, pattern string) (int64, error)
}
