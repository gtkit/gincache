package gincache

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gtkit/gincache/persist"
)

// TestHandlerPanicDoesNotBreakRecovery 钉住 handler panic 时 gin.Recovery 仍可用。
//
// 缺陷形态：包装器的回收先于 c.Writer 的恢复。panic 时恢复那步执行不到，而回收
// 已把包装器的内层 ResponseWriter 置 nil 并放回池——Recovery 拿着仍指向包装器的
// c.Writer 写 500 就是二次 panic；且包装器已回池，可能被另一个请求取走，
// 两个请求共用一个 writer。
func TestHandlerPanicDoesNotBreakRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := persist.NewMemoryStore(time.Minute)
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(Cache(store, time.Minute,
		WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
			return true, Strategy{CacheKey: c.Request.URL.Path}
		}),
	))
	engine.GET("/boom", func(*gin.Context) { panic("handler exploded") })

	recorder := httptest.NewRecorder()
	// 不得 panic 逃出 ServeHTTP：Recovery 必须能正常写出 500。
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500：Recovery 没能写出响应", recorder.Code)
	}
}

// TestPanicDoesNotCacheAnything 钉住 panic 的请求不留下缓存。
//
// panic 时响应是 Recovery 写的 500，不是 handler 的产物；把它缓存下来会让
// 后续请求在 TTL 内都拿到这个 500，而故障可能早已过去。
func TestPanicDoesNotCacheAnything(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := persist.NewMemoryStore(time.Minute)
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(Cache(store, time.Minute,
		WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
			return true, Strategy{CacheKey: c.Request.URL.Path}
		}),
	))

	calls := 0
	engine.GET("/boom", func(*gin.Context) {
		calls++
		panic("handler exploded")
	})

	for range 2 {
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))
	}

	if calls != 2 {
		t.Fatalf("handler 被调用 %d 次，want 2：panic 产生的 500 被缓存了", calls)
	}
}
