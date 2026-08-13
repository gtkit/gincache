// Package persist 提供缓存存储后端接口和实现
package persist

import (
	"context"
	"errors"
	"math"
	"reflect"
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

// isNilValue 报告一个接口值是否为 nil，包括装进接口的 typed-nil。
//
// `var client *redis.Client` 装进接口之后接口本身非 nil，只查 `== nil` 会把它放
// 过去，错误要拖到第一次读写才以 nil 解引用的形式炸出来——而且在 singleflight
// 内炸会连累所有等待者。构造期就该拦下。
//
// 主包 cache.go 的 isNilStore 是同一形状的判定。两者分处不同包，共用需要把它
// 导出，而一个 nil 判定对调用方没有价值，因此各自保留一份小实现。
func isNilValue(value any) bool {
	if value == nil {
		return true
	}

	switch rv := reflect.ValueOf(value); rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.Slice, reflect.UnsafePointer, reflect.Interface:
		return rv.IsNil()
	default:
		return false
	}
}

// countAsInt64 把无符号计数器转成 int64，超出可表示范围时钳到最大值。
//
// 统计接口对外是 map[string]int64，而计数器内部是 atomic.Uint64。溢出需要
// 9.2×10^18 次操作，现实中到不了；钳制是为了让转换本身不带未定义行为，
// 也让安全扫描不必对每处转换都提出疑问。
func countAsInt64(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}
