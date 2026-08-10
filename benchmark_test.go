package gincache

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gtkit/gincache/persist"
)

// benchPayload 是三个基准共用的 handler，产出一个有代表性的 JSON 响应。
func benchPayload(c *gin.Context) {
	c.Header("ETag", "e1")
	c.Header("Cache-Control", "public, max-age=60")
	c.JSON(http.StatusOK, gin.H{"id": 1, "name": "widget", "tags": []string{"a", "b"}})
}

func benchEngine(b *testing.B, store persist.CacheStore) *gin.Engine {
	b.Helper()
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(CacheByRequestPath(store, time.Minute))
	engine.GET("/x", benchPayload)
	return engine
}

// BenchmarkCacheHit 测量缓存命中回放的开销。
func BenchmarkCacheHit(b *testing.B) {
	store := persist.NewMemoryStore(time.Minute)
	b.Cleanup(func() { _ = store.Close() })

	engine := benchEngine(b, store)
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	engine.ServeHTTP(httptest.NewRecorder(), req) // 预热写入

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		engine.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// BenchmarkCacheHitParallel 测量并发命中，覆盖回放路径上的共享数据竞争。
func BenchmarkCacheHitParallel(b *testing.B) {
	store := persist.NewMemoryStore(time.Minute)
	b.Cleanup(func() { _ = store.Close() })

	engine := benchEngine(b, store)
	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		for pb.Next() {
			engine.ServeHTTP(httptest.NewRecorder(), req)
		}
	})
}

// BenchmarkCacheMissAndStore 测量未命中回源加回填的完整开销。
//
// 用恒未命中的 store 而不是每轮构造唯一缓存键：后者会把 URL 拼接和请求构造的
// 成本算进来，量到的就不是缓存路径本身了。
func BenchmarkCacheMissAndStore(b *testing.B) {
	inner := persist.NewMemoryStore(time.Minute)
	b.Cleanup(func() { _ = inner.Close() })

	engine := benchEngine(b, alwaysMissStore{inner})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		engine.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// alwaysMissStore 恒未命中，写入照常走真实实现。
type alwaysMissStore struct {
	persist.CacheStore
}

func (alwaysMissStore) Get(string, any) error {
	return persist.ErrCacheMiss
}
