package gincache

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gtkit/gincache/persist"
)

// TestCacheableResponseBlocksLeakyResponses 钉住响应期判据能挡住三类泄漏。
//
// 本包会把响应头连同 Body 一起缓存并在命中时回放（只排除 X-Cache）。因此
// Set-Cookie 会被发给后续所有请求、Cache-Control: private 被无视、Vary 被
// 忽略——而请求期判据看不到响应头，无法表达这些约束。
func TestCacheableResponseBlocksLeakyResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)

	reject := func(_ int, header http.Header) bool {
		if len(header.Values("Set-Cookie")) > 0 {
			return false
		}
		if header.Get("Vary") != "" {
			return false
		}
		switch header.Get("Cache-Control") {
		case "private", "no-store":
			return false
		}
		return true
	}

	tests := []struct {
		name      string
		setHeader func(*gin.Context)
		wantCalls int32
	}{
		{
			name:      "Set-Cookie 不进缓存",
			setHeader: func(c *gin.Context) { c.Header("Set-Cookie", "session=abc") },
			wantCalls: 2,
		},
		{
			name:      "Cache-Control private 不进缓存",
			setHeader: func(c *gin.Context) { c.Header("Cache-Control", "private") },
			wantCalls: 2,
		},
		{
			name:      "Vary 不进缓存",
			setHeader: func(c *gin.Context) { c.Header("Vary", "Accept-Language") },
			wantCalls: 2,
		},
		{
			name:      "普通响应正常缓存",
			setHeader: func(*gin.Context) {},
			wantCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			store := persist.NewMemoryStore(time.Minute)
			engine := gin.New()
			engine.Use(Cache(store, time.Minute,
				WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
					return true, Strategy{CacheKey: c.Request.URL.Path}
				}),
				WithCacheableResponse(reject),
			))
			engine.GET("/x", func(c *gin.Context) {
				calls.Add(1)
				tt.setHeader(c)
				c.JSON(http.StatusOK, gin.H{"ok": true})
			})

			for range 2 {
				engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
			}

			if got := calls.Load(); got != tt.wantCalls {
				t.Fatalf("handler 被调用 %d 次，want %d", got, tt.wantCalls)
			}
		})
	}
}

// TestCacheableResponseDefaultsToBaseline 钉住不设该选项时内置基线生效。
//
// 把安全默认做成 opt-in 等于让每个调用方各自记得配置一遍，漏一个就是会话泄漏。
// 基线只挡 RFC 9111 对共享缓存明确禁止的三类，误伤面接近零。
func TestCacheableResponseDefaultsToBaseline(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	store := persist.NewMemoryStore(time.Minute)
	engine := gin.New()
	engine.Use(Cache(store, time.Minute,
		WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
			return true, Strategy{CacheKey: c.Request.URL.Path}
		}),
	))
	engine.GET("/x", func(c *gin.Context) {
		calls.Add(1)
		c.Header("Set-Cookie", "session=abc")
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	for range 2 {
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("handler 被调用 %d 次，want 2（未设判据时内置基线应挡住 Set-Cookie）", got)
	}
}

// TestCacheableResponseReplacesBaseline 钉住自定义判据替换而非叠加基线。
//
// 叠加会堵死一个合法用法：缓存键已含用户维度时，缓存 private 响应是正确的。
func TestCacheableResponseReplacesBaseline(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	store := persist.NewMemoryStore(time.Minute)
	engine := gin.New()
	engine.Use(Cache(store, time.Minute,
		WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
			return true, Strategy{CacheKey: c.Request.URL.Path}
		}),
		WithCacheableResponse(func(int, http.Header) bool { return true }),
	))
	engine.GET("/x", func(c *gin.Context) {
		calls.Add(1)
		c.Header("Cache-Control", "private")
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	for range 2 {
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("handler 被调用 %d 次，want 1（自定义判据应完整替换基线）", got)
	}
}

// TestCacheableResponseCannotMutateCache 钉住判据拿到的是副本。
//
// http.Header 是 map，把内部实例交给回调等于允许它改写随后写入 store 的内容——
// 一个只该"判断"的回调却能改数据，而调用方不会意识到自己有这个能力。
func TestCacheableResponseCannotMutateCache(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := persist.NewMemoryStore(time.Minute)
	engine := gin.New()
	engine.Use(Cache(store, time.Minute,
		WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
			return true, Strategy{CacheKey: c.Request.URL.Path}
		}),
		WithCacheableResponse(func(_ int, header http.Header) bool {
			// 判据试图注入一个头：它只该看，不该改。
			header.Set("X-Injected-By-Predicate", "yes")
			return true
		}),
	))
	engine.GET("/x", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// 第一次回源并写缓存，第二次命中回放。
	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	second := httptest.NewRecorder()
	engine.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/x", nil))

	if got := second.Header().Get("X-Injected-By-Predicate"); got != "" {
		t.Fatalf("判据注入的头进了缓存并被回放（值 %q）：它拿到的不是副本", got)
	}
}
