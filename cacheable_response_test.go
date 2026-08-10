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

// TestCacheableResponseDefaultsToStatusOnly 钉住不设该选项时行为不变。
//
// 这是向后兼容的要求：既有调用方没有这个判据，升级后必须与从前完全一致。
func TestCacheableResponseDefaultsToStatusOnly(t *testing.T) {
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
		// 不设判据时，即使带 Set-Cookie 也照旧缓存——这正是引入本选项要修的
		// 问题，但默认行为必须保持不变，否则升级会静默改变既有调用方的语义。
		c.Header("Set-Cookie", "session=abc")
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	for range 2 {
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("handler 被调用 %d 次，want 1（未设判据时行为应与从前一致）", got)
	}
}
