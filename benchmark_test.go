package gincache

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gtkit/gincache/persist"
)

func benchEngine(b *testing.B) *gin.Engine {
	b.Helper()
	gin.SetMode(gin.ReleaseMode)
	store := persist.NewMemoryStore(time.Minute)
	b.Cleanup(func() { _ = store.Close() })
	engine := gin.New()
	engine.Use(CacheByRequestPath(store, time.Minute))
	engine.GET("/x", func(c *gin.Context) {
		c.Header("ETag", "e1")
		c.Header("Cache-Control", "public, max-age=60")
		c.JSON(http.StatusOK, gin.H{"id": 1, "name": "widget", "tags": []string{"a", "b"}})
	})
	return engine
}

func BenchmarkCacheHit(b *testing.B) {
	engine := benchEngine(b)
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	engine.ServeHTTP(httptest.NewRecorder(), req) // 预热写入
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		engine.ServeHTTP(httptest.NewRecorder(), req)
	}
}

func BenchmarkCacheMiss(b *testing.B) {
	gin.SetMode(gin.ReleaseMode)
	store := persist.NewMemoryStore(time.Millisecond)
	b.Cleanup(func() { _ = store.Close() })
	engine := gin.New()
	engine.Use(Cache(store, time.Millisecond,
		WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
			return false, Strategy{}
		}),
	))
	engine.GET("/x", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"id": 1}) })
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		engine.ServeHTTP(httptest.NewRecorder(), req)
	}
}
