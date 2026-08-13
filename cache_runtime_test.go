package gincache

import (
	"context"
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

// TestSingleFlightIsolatesStores 钉住不同存储不会被同一条 flight 串在一起。
//
// Strategy.CacheStore 允许逐请求换存储（典型用法是按租户分库），而 flight 只按
// CacheKey 合并的话，两个键相同但存储不同的并发请求会共享 leader 的响应——
// 按存储做的隔离就被从背后打穿了。
func TestSingleFlightIsolatesStores(t *testing.T) {
	gin.SetMode(gin.TestMode)

	storeA := persist.NewMemoryStore(time.Minute)
	storeB := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = storeA.Close(); _ = storeB.Close() })

	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})

	engine := gin.New()
	engine.Use(Cache(storeA, time.Minute,
		WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
			// 两个租户共用同一个缓存键，只靠存储区分。
			strategy := Strategy{CacheKey: "shared-key", CacheStore: storeA}
			if c.GetHeader("X-Tenant") == "B" {
				strategy.CacheStore = storeB
			}
			return true, strategy
		}),
	))
	engine.GET("/x", func(c *gin.Context) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		c.String(http.StatusOK, "tenant-%s", c.GetHeader("X-Tenant"))
	})

	serve := func(tenant string) string {
		request := httptest.NewRequest(http.MethodGet, "/x", nil)
		request.Header.Set("X-Tenant", tenant)
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		return recorder.Body.String()
	}

	bodies := make([]string, 2)
	var wg sync.WaitGroup
	wg.Go(func() { bodies[0] = serve("A") })
	<-entered
	wg.Go(func() { bodies[1] = serve("B") })

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if bodies[0] == bodies[1] {
		t.Fatalf("两个租户拿到同一份响应 %q：singleflight 打穿了存储隔离", bodies[0])
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler 被调用 %d 次，want 2", got)
	}
}

// TestRequestNoStoreSkipsWrite 钉住请求带 Cache-Control: no-store 时不写入缓存。
//
// 与请求端的 no-cache 不同层级：no-cache 管"能不能复用"，遵守它会放开缓存击穿；
// no-store 只管"能不能写"，已有条目照常命中，制造不出别人的 miss（RFC 9111 §5.2.1.5）。
func TestRequestNoStoreSkipsWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("不写入缓存", func(t *testing.T) {
		var calls atomic.Int32
		store := persist.NewMemoryStore(time.Minute)
		t.Cleanup(func() { _ = store.Close() })

		engine := admissionEngine(store)
		engine.GET("/x", func(c *gin.Context) {
			calls.Add(1)
			c.String(http.StatusOK, "ok")
		})

		for range 2 {
			request := httptest.NewRequest(http.MethodGet, "/x", nil)
			request.Header.Set("Cache-Control", "no-store")
			engine.ServeHTTP(httptest.NewRecorder(), request)
		}

		if got := calls.Load(); got != 2 {
			t.Fatalf("handler 被调用 %d 次，want 2：带 no-store 的响应被写入了缓存", got)
		}
		var cached ResponseCache
		if err := store.Get("/x", &cached); err == nil {
			t.Fatal("带 no-store 的请求把响应写进了 store")
		}
	})

	t.Run("已有条目仍可命中", func(t *testing.T) {
		var calls atomic.Int32
		store := persist.NewMemoryStore(time.Minute)
		t.Cleanup(func() { _ = store.Close() })

		engine := admissionEngine(store)
		engine.GET("/x", func(c *gin.Context) {
			calls.Add(1)
			c.String(http.StatusOK, "ok")
		})

		// 先用普通请求把条目写进去。
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

		request := httptest.NewRequest(http.MethodGet, "/x", nil)
		request.Header.Set("Cache-Control", "no-store")
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)

		if got := recorder.Header().Get("X-Cache"); got != "HIT" {
			t.Fatalf("X-Cache = %q, want HIT：no-store 管的是写，不该连读也挡掉", got)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("handler 被调用 %d 次，want 1", got)
		}
	})

	t.Run("非规范键的 cache-control 同样生效", func(t *testing.T) {
		var calls atomic.Int32
		store := persist.NewMemoryStore(time.Minute)
		t.Cleanup(func() { _ = store.Close() })

		engine := admissionEngine(store)
		engine.GET("/x", func(c *gin.Context) {
			calls.Add(1)
			c.String(http.StatusOK, "ok")
		})

		for range 2 {
			request := httptest.NewRequest(http.MethodGet, "/x", nil)
			request.Header["cache-control"] = []string{"no-store"}
			engine.ServeHTTP(httptest.NewRecorder(), request)
		}

		if got := calls.Load(); got != 2 {
			t.Fatalf("handler 被调用 %d 次，want 2", got)
		}
	})
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

	// 归零后不应再创建定时器，stop 是可安全调用的空函数，且没有定时器会去置位
	// 淘汰标记。
	superseded, stop := m.scheduleForget("k")
	stop()
	if superseded.Load() {
		t.Fatal("未安排定时器时不应置位淘汰标记")
	}
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

// TestUncomparableValueStoreDoesNotPanic 钉住值类型存储不会打崩中间件。
//
// 用接口值互相比较来判断"是不是默认存储"，对不可比较的动态类型（带切片字段的
// 值类型）会直接 panic——为了修跨租户泄漏而引入一个更糟的失败模式。
func TestUncomparableValueStoreDoesNotPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	inner := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = inner.Close() })
	store := uncomparableStore{inner: inner, tags: []string{"a"}}

	engine := gin.New()
	engine.Use(Cache(store, time.Minute,
		WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
			return true, Strategy{CacheKey: c.Request.URL.Path, CacheStore: store}
		}),
	))
	engine.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/x", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}

// TestTypedNilStoreRejected 钉住 typed-nil 存储在构造期就被拒。
//
// `var store *persist.MemoryStore` 装进接口之后接口本身非 nil，只查 store == nil
// 会放它过去，错误要拖到第一次读写才以 nil 解引用的形式炸出来。
func TestTypedNilStoreRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("New 构造期 panic", func(t *testing.T) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				t.Fatal("typed-nil store 没有 panic")
			}
			if msg, _ := recovered.(string); msg != "gincache: store must not be nil" {
				t.Fatalf("panic 信息 = %v", recovered)
			}
		}()
		var store *persist.MemoryStore
		New(store, time.Minute)
	})

	t.Run("Strategy 里的 typed-nil 不缓存也不退回默认存储", func(t *testing.T) {
		var calls atomic.Int32
		base := persist.NewMemoryStore(time.Minute)
		t.Cleanup(func() { _ = base.Close() })

		engine := gin.New()
		engine.Use(Cache(base, time.Minute,
			WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
				var broken *persist.MemoryStore
				return true, Strategy{CacheKey: c.Request.URL.Path, CacheStore: broken}
			}),
		))
		engine.GET("/x", func(c *gin.Context) {
			calls.Add(1)
			c.String(http.StatusOK, "ok")
		})

		for range 2 {
			engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
		}

		if got := calls.Load(); got != 2 {
			t.Fatalf("handler 被调用 %d 次，want 2", got)
		}
		var cached ResponseCache
		if err := base.Get("/x", &cached); err == nil {
			t.Fatal("typed-nil 的 Strategy.CacheStore 让响应落进了默认存储")
		}
	})
}

// uncomparableStore 是一个以值类型实现接口、且因含切片字段而不可比较的存储。
type uncomparableStore struct {
	inner *persist.MemoryStore
	tags  []string
}

func (s uncomparableStore) Get(key string, value any) error { return s.inner.Get(key, value) }

func (s uncomparableStore) Set(key string, value any, expire time.Duration) error {
	return s.inner.Set(key, value, expire)
}

func (s uncomparableStore) Delete(key string) error { return s.inner.Delete(key) }

func (s uncomparableStore) SetWithContext(
	ctx context.Context, key string, value any, expire time.Duration,
) error {
	return s.inner.SetWithContext(ctx, key, value, expire)
}
