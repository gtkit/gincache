// Package bigcacheadapter 提供按需引入的本地缓存适配器。
package bigcacheadapter

import (
	"github.com/gtkit/gincache/persist/internal/localadapter"
	"time"
)

// Owner 管理一个原生实例；共享消费者使用不同 namespace。
type Owner = localadapter.Owner

// NamespaceStore 是隔离视图，实现 persist.CacheStore 和 persist.LocalStore。
type NamespaceStore = localadapter.Store

// StoreOption 配置 namespace 视图，与旧 Ristretto Option 独立。
type StoreOption = localadapter.Option

var (
	// ErrInvalidArgument 表示无效构造参数或 TTL。
	ErrInvalidArgument = localadapter.ErrInvalidArgument
	// ErrClosed 表示 Owner 或视图已关闭。
	ErrClosed = localadapter.ErrClosed
	// ErrCorruptEntry 表示缓存记录损坏。
	ErrCorruptEntry = localadapter.ErrCorruptEntry
)

// WithStoreExpiration 设置视图零 TTL 的默认值；默认零为无逻辑期限。
func WithStoreExpiration(ttl time.Duration) StoreOption {
	return localadapter.WithDefaultExpiration(ttl)
}

// WithClock 设置视图逻辑时钟；默认 time.Now。
func WithClock(now func() time.Time) StoreOption { return localadapter.WithClock(now) }
