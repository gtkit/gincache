package gincache

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gtkit/gincache/persist"
)

// TestCacheKeyIsolatesMethod 钉住内置中间件的缓存键隔离 HTTP method。
//
// 不含 method 的键会让挂在 r.Any() 或混方法路由组下的 POST 复用 GET 的响应。
func TestCacheKeyIsolatesMethod(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = store.Close() })

	router := gin.New()
	router.Any("/x", CacheByRequestPath(store, time.Minute), func(c *gin.Context) {
		calls.Add(1)
		c.String(http.StatusOK, "%s response", c.Request.Method)
	})

	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/x", nil))
	if got := first.Body.String(); got != "GET response" {
		t.Fatalf("GET body = %q", got)
	}

	second := httptest.NewRecorder()
	router.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/x", nil))
	if got := second.Body.String(); got != "POST response" {
		t.Fatalf("POST body = %q, want %q（POST 复用了 GET 的缓存）", got, "POST response")
	}
	if got := second.Header().Get("X-Cache"); got == "HIT" {
		t.Fatal("POST 命中了 GET 写入的缓存条目")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler 被调用 %d 次，want 2", got)
	}
}

// TestCacheKeySameMethodStillHits 钉住同 method 的缓存命中不受影响。
func TestCacheKeySameMethodStillHits(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = store.Close() })

	router := gin.New()
	router.GET("/x", CacheByRequestURI(store, time.Minute), func(c *gin.Context) {
		calls.Add(1)
		c.String(http.StatusOK, "payload")
	})

	for range 2 {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("handler 被调用 %d 次，want 1", got)
	}
}

// TestCacheKeyHeadDoesNotPoisonGet 钉住 HEAD 先到不会污染 GET 条目。
//
// 曾经把 HEAD 归一为 GET，只顾了读方向：HEAD 首次未命中会把 HEAD 专属响应
// （这里的 handler 在 HEAD 分支不产出 Body，是最常见的写法）写进 GET 键，
// 随后的 GET 命中一个空条目。
func TestCacheKeyHeadDoesNotPoisonGet(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = store.Close() })

	router := gin.New()
	handler := func(c *gin.Context) {
		calls.Add(1)
		c.Header("ETag", "etag-1")
		if c.Request.Method == http.MethodHead {
			c.Status(http.StatusOK)
			return
		}
		c.String(http.StatusOK, "payload")
	}
	router.GET("/x", CacheByRequestPath(store, time.Minute), handler)
	router.HEAD("/x", CacheByRequestPath(store, time.Minute), handler)

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodHead, "/x", nil))

	get := httptest.NewRecorder()
	router.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/x", nil))

	if got := get.Header().Get("X-Cache"); got == "HIT" {
		t.Fatal("GET 命中了 HEAD 写入的条目")
	}
	if got := get.Body.String(); got != "payload" {
		t.Fatalf("GET body = %q, want %q（拿到了 HEAD 写下的空条目）", got, "payload")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler 被调用 %d 次，want 2", got)
	}
}

// TestCacheKeyHeadHitsOwnEntry 钉住 HEAD 命中自己写入的条目且不写出 Body。
func TestCacheKeyHeadHitsOwnEntry(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = store.Close() })

	router := gin.New()
	router.HEAD("/x", CacheByRequestPath(store, time.Minute), func(c *gin.Context) {
		calls.Add(1)
		c.Header("ETag", "etag-1")
		c.String(http.StatusOK, "payload")
	})

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodHead, "/x", nil))

	head := httptest.NewRecorder()
	router.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/x", nil))

	if got := head.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("HEAD X-Cache = %q, want HIT", got)
	}
	if head.Body.Len() != 0 {
		t.Fatalf("HEAD body 长度 = %d, want 0", head.Body.Len())
	}
	if got := head.Header().Get("ETag"); got != "etag-1" {
		t.Fatalf("HEAD ETag = %q, want etag-1", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler 被调用 %d 次，want 1", got)
	}
}

// TestUnsafeMethodsNotCached 钉住内置中间件不缓存非安全方法。
//
// 缓存一次 POST 的响应并回放，等于让后续同键请求跳过业务处理——副作用不会
// 发生，调用方却收到成功响应。
func TestUnsafeMethodsNotCached(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			var calls atomic.Int32
			store := persist.NewMemoryStore(time.Minute)
			t.Cleanup(func() { _ = store.Close() })

			router := gin.New()
			router.Handle(method, "/order", CacheByRequestPath(store, time.Minute), func(c *gin.Context) {
				n := calls.Add(1)
				c.JSON(http.StatusOK, gin.H{"order": n})
			})

			var last string
			for range 2 {
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(method, "/order", nil))
				if got := rec.Header().Get("X-Cache"); got == "HIT" {
					t.Fatalf("%s 命中了缓存", method)
				}
				last = rec.Body.String()
			}

			if got := calls.Load(); got != 2 {
				t.Fatalf("handler 被调用 %d 次，want 2：%s 的响应被缓存并跳过了业务处理", got, method)
			}
			if last != `{"order":2}` {
				t.Fatalf("第二次响应 = %s, want {\"order\":2}", last)
			}
		})
	}
}

// TestAuthorizedRequestBypassesBuiltinCache 钉住带 Authorization 的请求绕过内置中间件。
//
// 内置键不含任何请求头，无从区分不同凭据的用户；RFC 9111 §3.5 也规定共享缓存
// 不得复用带 Authorization 请求的响应。
func TestAuthorizedRequestBypassesBuiltinCache(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newRouter := func(store persist.CacheStore, calls *atomic.Int32) *gin.Engine {
		router := gin.New()
		router.GET("/me", CacheByRequestPath(store, time.Minute), func(c *gin.Context) {
			n := calls.Add(1)
			c.JSON(http.StatusOK, gin.H{"user": n})
		})
		return router
	}

	t.Run("不读缓存", func(t *testing.T) {
		var calls atomic.Int32
		store := persist.NewMemoryStore(time.Minute)
		t.Cleanup(func() { _ = store.Close() })
		router := newRouter(store, &calls)

		// 先用匿名请求把条目写进缓存。
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/me", nil))

		authed := httptest.NewRequest(http.MethodGet, "/me", nil)
		authed.Header.Set("Authorization", "Bearer token-a")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, authed)

		if got := rec.Header().Get("X-Cache"); got == "HIT" {
			t.Fatal("带 Authorization 的请求命中了共享缓存")
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("handler 被调用 %d 次，want 2", got)
		}
	})

	t.Run("非规范键的 authorization 同样绕过", func(t *testing.T) {
		var calls atomic.Int32
		store := persist.NewMemoryStore(time.Minute)
		t.Cleanup(func() { _ = store.Close() })
		router := newRouter(store, &calls)

		// 直接写 map，绕过 Header.Set 的规范化——真实流量不会这样，
		// 但测试、内部中间件、网关适配层都可能这样构造请求。
		for range 2 {
			req := httptest.NewRequest(http.MethodGet, "/me", nil)
			req.Header["authorization"] = []string{"Bearer token-a"}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if got := rec.Header().Get("X-Cache"); got == "HIT" {
				t.Fatal("非规范键的 authorization 绕过了门禁并命中了共享缓存")
			}
		}

		if got := calls.Load(); got != 2 {
			t.Fatalf("handler 被调用 %d 次，want 2", got)
		}
	})

	t.Run("不写缓存", func(t *testing.T) {
		var calls atomic.Int32
		store := persist.NewMemoryStore(time.Minute)
		t.Cleanup(func() { _ = store.Close() })
		router := newRouter(store, &calls)

		authed := httptest.NewRequest(http.MethodGet, "/me", nil)
		authed.Header.Set("Authorization", "Bearer token-a")
		router.ServeHTTP(httptest.NewRecorder(), authed)

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/me", nil))

		if got := rec.Header().Get("X-Cache"); got == "HIT" {
			t.Fatal("匿名请求命中了带 Authorization 请求写下的条目")
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("handler 被调用 %d 次，want 2", got)
		}
	})
}

// TestCustomStrategyNotGatedByRequest 钉住请求维度门禁只作用于内置中间件。
//
// 走 Cache + WithCacheStrategyByRequest 的调用方已经显式表达了"缓存这个请求"，
// 那是他们的判断，本包不代为否决。
func TestCustomStrategyNotGatedByRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = store.Close() })

	router := gin.New()
	router.POST("/search", Cache(store, time.Minute,
		WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
			return true, Strategy{CacheKey: "search:" + c.GetHeader("Authorization")}
		}),
	), func(c *gin.Context) {
		calls.Add(1)
		c.JSON(http.StatusOK, gin.H{"hits": 1})
	})

	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/search", nil)
		req.Header.Set("Authorization", "Bearer token-a")
		router.ServeHTTP(httptest.NewRecorder(), req)
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("handler 被调用 %d 次，want 1：自定义策略不该受内置请求门禁限制", got)
	}
}

// TestSingleFlightForgetTimerBoundToFlight 钉住已结束请求的定时器不再影响后续 flight。
//
// 定时器原本建在 Do 之外且从不 Stop：早已结束的请求，其定时器会在毫不相干的
// 后续 flight 期间 Forget，同一个 key 于是出现多个 leader，防击穿失效。
func TestSingleFlightForgetTimerBoundToFlight(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 时间线（forgetTimeout = 300ms）：
	//   t=0    请求 A 快速结束——缺陷版本会给这个 key 留下一个 t=300ms 触发的定时器
	//   t=250  请求 B 进入并阻塞，它自己的定时器指向 t=550ms
	//   t=400  请求 C 进入：缺陷版本此时 key 已被 A 的残留定时器 Forget，C 成为第二个
	//          leader；修复后 C 应当并入 B 的 flight
	//   t=450  放行 B
	const forgetTimeout = 300 * time.Millisecond

	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})

	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = store.Close() })

	engine := gin.New()
	// TTL 取 100ms：A 写下的条目要在 B 进入前过期，否则 B 直接命中缓存、
	// 根本不会进入 singleflight。响应本身保持可缓存，这样 C 才可能并入 B 的 flight。
	engine.Use(Cache(store, 100*time.Millisecond,
		WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
			return true, Strategy{CacheKey: c.Request.URL.Path}
		}),
		WithSingleFlightForgetTimeout(forgetTimeout),
	))
	engine.GET("/x", func(c *gin.Context) {
		n := calls.Add(1)
		if n == 2 {
			close(entered)
			<-release
		}
		c.String(http.StatusOK, "r%d", n)
	})

	serve := func() {
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	}

	serve() // 请求 A
	time.Sleep(250 * time.Millisecond)

	var wg sync.WaitGroup
	wg.Go(serve) // 请求 B
	<-entered

	time.Sleep(150 * time.Millisecond) // 越过 A 的残留定时器触发点
	wg.Go(serve)                       // 请求 C

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 2 {
		t.Fatalf("handler 被调用 %d 次，want 2：已结束请求的定时器 Forget 了进行中的 flight", got)
	}
}

// TestOptionsDefendAgainstNegativeValues 钉住 Option 对负数入参的防御。
func TestOptionsDefendAgainstNegativeValues(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = store.Close() })

	m := New(store, time.Minute,
		WithMaxBodySize(-1),
		WithSingleFlightForgetTimeout(-5*time.Second),
	)

	if m.cfg.maxBodySize != 0 {
		t.Fatalf("maxBodySize = %d, want 0（负数应被忽略）", m.cfg.maxBodySize)
	}
	if m.cfg.singleFlightForgetTimeout != 0 {
		t.Fatalf("singleFlightForgetTimeout = %v, want 0（负数应被归零）", m.cfg.singleFlightForgetTimeout)
	}

	// 归零后不应再创建定时器，stop 是可安全调用的空函数。
	m.scheduleForget("k")()
}

// TestNewValidatesConfiguration 钉住配置错误在构造期而不是请求期暴露。
func TestNewValidatesConfiguration(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("nil store 构造期 panic", func(t *testing.T) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				t.Fatal("nil store 没有 panic")
			}
			if msg, _ := recovered.(string); msg != "gincache: store must not be nil" {
				t.Fatalf("panic 信息 = %v，未指明 store 不能为 nil", recovered)
			}
		}()
		New(nil, time.Minute)
	})

	t.Run("负数默认 TTL 构造期 panic", func(t *testing.T) {
		store := persist.NewMemoryStore(time.Minute)
		t.Cleanup(func() { _ = store.Close() })

		defer func() {
			recovered := recover()
			if recovered == nil {
				t.Fatal("负数 defaultExpire 没有 panic")
			}
			if msg, _ := recovered.(string); msg != "gincache: defaultExpire must not be negative" {
				t.Fatalf("panic 信息 = %v，未指明 defaultExpire 不能为负数", recovered)
			}
		}()
		New(store, -time.Second)
	})

	t.Run("nil Option 被跳过", func(t *testing.T) {
		store := persist.NewMemoryStore(time.Minute)
		t.Cleanup(func() { _ = store.Close() })

		m := New(store, time.Minute, nil, WithMaxBodySize(1024), nil)
		if m.cfg.maxBodySize != 1024 {
			t.Fatalf("maxBodySize = %d, want 1024（nil Option 应被跳过而非中断）", m.cfg.maxBodySize)
		}
	})
}
