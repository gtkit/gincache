package gincache

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gtkit/gincache/persist"
)

// TestSingleFlightSharingMatrix 把"leader 的响应能不能交给并发等待者"的全部理由
// 摆进一张表里一次测完。
//
// 这块逻辑此前是一次修一个理由：no-store、Body 超限被丢弃、handler 什么都没写、
// 准入基线拒绝、新鲜度不合格、状态码不可缓存——每一条都单独出过事，而且改完
// 一处往往漏掉下一处。单点补正是反复出问题的原因，所以把它们钉在一起：
// 新增任何一条"不可共享"的理由，都必须在这张表里有一行。
//
// 判据是二选一的：等待者要么拿到 leader 的响应（X-Cache: HIT，handler 只跑一次），
// 要么各自回源（无 X-Cache，handler 跑两次）。中间状态都是 bug。
func TestSingleFlightSharingMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name string
		// opts 作用于整个中间件。
		opts []Option
		// decorate 定制 leader 那个请求；等待者始终是一个普通请求。
		decorate func(*http.Request)
		// respond 是 leader 与等待者共用的 handler 主体。
		respond func(*gin.Context)
		// wantShared 表示等待者是否应当拿到 leader 的响应。
		wantShared bool
	}{
		{
			name:       "普通可缓存响应可以共享",
			respond:    func(c *gin.Context) { c.String(http.StatusOK, "payload") },
			wantShared: true,
		},
		{
			name:       "请求声明 no-store 时不共享",
			decorate:   func(r *http.Request) { r.Header.Set("Cache-Control", "no-store") },
			respond:    func(c *gin.Context) { c.String(http.StatusOK, "payload") },
			wantShared: false,
		},
		{
			name:       "Body 超过上限被丢弃时不共享",
			opts:       []Option{WithMaxBodySize(4)},
			respond:    func(c *gin.Context) { c.String(http.StatusOK, "0123456789") },
			wantShared: false,
		},
		{
			name:       "handler 什么都没写时不共享",
			respond:    func(*gin.Context) {},
			wantShared: false,
		},
		{
			name: "准入基线拒绝的响应不共享",
			respond: func(c *gin.Context) {
				c.Header("Set-Cookie", "session=abc")
				c.String(http.StatusOK, "payload")
			},
			wantShared: false,
		},
		{
			name: "Vary 星号的响应不共享",
			respond: func(c *gin.Context) {
				c.Header("Vary", "*")
				c.String(http.StatusOK, "payload")
			},
			wantShared: false,
		},
		{
			name: "新鲜度不合格的响应不共享",
			respond: func(c *gin.Context) {
				c.Header("Cache-Control", "max-age=60")
				c.Header("Age", "120")
				c.String(http.StatusOK, "payload")
			},
			wantShared: false,
		},
		{
			name: "状态码不可缓存的响应不共享",
			respond: func(c *gin.Context) {
				c.Data(http.StatusPartialContent, "text/plain", []byte("part"))
			},
			wantShared: false,
		},
		{
			name: "显式声明可缓存的新鲜响应可以共享",
			respond: func(c *gin.Context) {
				c.Header("Cache-Control", "public, max-age=600")
				c.String(http.StatusOK, "payload")
			},
			wantShared: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			entered := make(chan struct{})
			release := make(chan struct{})

			store := persist.NewMemoryStore(time.Minute)
			t.Cleanup(func() { _ = store.Close() })

			engine := admissionEngine(store, tt.opts...)
			engine.GET("/x", func(c *gin.Context) {
				if calls.Add(1) == 1 {
					close(entered)
					<-release
				}
				tt.respond(c)
			})

			leader := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tt.decorate != nil {
				tt.decorate(leader)
			}

			waiterRecorder := httptest.NewRecorder()
			var wg sync.WaitGroup
			wg.Go(func() { engine.ServeHTTP(httptest.NewRecorder(), leader) })
			<-entered
			wg.Go(func() {
				engine.ServeHTTP(waiterRecorder, httptest.NewRequest(http.MethodGet, "/x", nil))
			})

			// 给等待者足够时间进入 singleflight 的等待队列。
			time.Sleep(50 * time.Millisecond)
			close(release)
			wg.Wait()

			shared := waiterRecorder.Header().Get("X-Cache") == "HIT"
			wantCalls := int32(2)
			if tt.wantShared {
				wantCalls = 1
			}

			if shared != tt.wantShared {
				t.Fatalf("等待者共享了 leader 的响应 = %v, want %v（X-Cache=%q）",
					shared, tt.wantShared, waiterRecorder.Header().Get("X-Cache"))
			}
			if got := calls.Load(); got != wantCalls {
				t.Fatalf("handler 被调用 %d 次，want %d：共享判定与实际执行次数不一致", got, wantCalls)
			}
		})
	}
}

// TestSingleFlightLeaderNotReexecuted 钉住 leader 不会因为"响应不可共享"而被二次执行。
//
// 不可共享时 flight 返回空，而类型断言失败的分支是 c.Next()——leader 若落进那一支，
// 整条 handler 链会再跑一遍，客户端拿到两份响应拼接的结果。
func TestSingleFlightLeaderNotReexecuted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = store.Close() })

	engine := admissionEngine(store)
	engine.GET("/x", func(c *gin.Context) {
		calls.Add(1)
		// 带 Set-Cookie，准入基线会拒绝，因此这份响应不可共享。
		c.Header("Set-Cookie", "session=abc")
		c.String(http.StatusOK, "once")
	})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/x", nil))

	if got := calls.Load(); got != 1 {
		t.Fatalf("handler 被调用 %d 次，want 1：leader 被二次执行", got)
	}
	if got := recorder.Body.String(); got != "once" {
		t.Fatalf("body = %q, want %q：响应被写了两次", got, "once")
	}
}

// TestFlightKeyNamespacesCannotCollide 钉住构造出的缓存键伪装不成另一类 flight 身份。
//
// 默认存储曾经直接用裸 key，自定义存储用"地址+NUL+key"——两个命名空间之间没有
// 分隔，调用方能自由决定 CacheKey，于是精心构造的键可以撞进另一个存储的 flight，
// 实测能拿到别的租户的响应。
func TestFlightKeyNamespacesCannotCollide(t *testing.T) {
	gin.SetMode(gin.TestMode)

	defaultStore := persist.NewMemoryStore(time.Minute)
	tenantStore := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = defaultStore.Close(); _ = tenantStore.Close() })

	// 复刻内部为指针类存储生成的身份，尝试用 CacheKey 伪装成它。
	forged := "p" + strconv.FormatUint(uint64(reflect.ValueOf(tenantStore).Pointer()), 36) + "\x00shared"

	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})

	engine := gin.New()
	engine.Use(Cache(defaultStore, time.Minute,
		WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
			if c.GetHeader("X-Tenant") != "" {
				return true, Strategy{CacheKey: "shared", CacheStore: tenantStore}
			}
			return true, Strategy{CacheKey: forged}
		}),
	))
	engine.GET("/x", func(c *gin.Context) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		c.String(http.StatusOK, "from-%s", c.DefaultQuery("who", "default"))
	})

	serve := func(query string, tenant bool) string {
		request := httptest.NewRequest(http.MethodGet, "/x?who="+query, nil)
		if tenant {
			request.Header.Set("X-Tenant", "a")
		}
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, request)
		return recorder.Body.String()
	}

	bodies := make([]string, 2)
	var wg sync.WaitGroup
	wg.Go(func() { bodies[0] = serve("tenant", true) })
	<-entered
	wg.Go(func() { bodies[1] = serve("default", false) })

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if bodies[0] == bodies[1] {
		t.Fatalf("两个存储拿到同一份响应 %q：构造出的键撞进了另一类 flight 身份", bodies[0])
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler 被调用 %d 次，want 2", got)
	}
}

// TestValueStoreKeepsSingleFlight 钉住值类型的默认存储仍然合并同键并发请求。
//
// 靠"取不到地址"判定为自定义存储会给每个请求发独占的 flight 身份，默认存储因此
// 彻底失去防击穿——而默认存储自始至终就是同一个实例，本该合并。
func TestValueStoreKeepsSingleFlight(t *testing.T) {
	gin.SetMode(gin.TestMode)

	inner := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = inner.Close() })

	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})

	engine := gin.New()
	engine.Use(Cache(uncomparableStore{inner: inner, tags: []string{"a"}}, time.Minute,
		WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
			return true, Strategy{CacheKey: c.Request.URL.Path}
		}),
	))
	engine.GET("/x", func(c *gin.Context) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		c.String(http.StatusOK, "payload")
	})

	var wg sync.WaitGroup
	wg.Go(func() {
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	})
	<-entered
	wg.Go(func() {
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	})

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("handler 被调用 %d 次，want 1：值类型的默认存储失去了防击穿", got)
	}
}

// TestExpiresWithoutDateNotDoubleCounted 钉住 Expires 无 Date 时驻留时间不被扣两遍。
//
// 回放时若拿当下当作"收到响应的时刻"，算出的已经是剩余时间，再与含驻留时长的
// 年龄一比，驻留时间就被扣了两次，条目在大约一半寿命处就失效。
func TestExpiresWithoutDateNotDoubleCounted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = store.Close() })

	engine := admissionEngine(store)
	engine.GET("/x", func(c *gin.Context) {
		calls.Add(1)
		// 只给 Expires，不给 Date。
		c.Header("Expires", time.Now().Add(3*time.Second).UTC().Format(http.TimeFormat))
		c.String(http.StatusOK, "payload")
	})

	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	// 过掉一半寿命：扣两遍的话这里就已经失效了。
	time.Sleep(1600 * time.Millisecond)

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/x", nil))

	if got := recorder.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("X-Cache = %q, want HIT：距 Expires 还有约 1.4 秒就失效了", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler 被调用 %d 次，want 1", got)
	}
}

// TestLegacyEntryAgeUsesStoredHeader 钉住旧格式条目回放时把条目里的 Age 纳入估算。
//
// 旧格式条目没有 ResponseTime / InitialAge，只能从响应头估算：Date 给出自源站生成
// 以来的总时长，条目里的 Age 是写入那一刻的年龄。只看 Date 会把一个上游已经很老
// 的响应报成 Age: 0。
func TestLegacyEntryAgeUsesStoredHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = store.Close() })

	// Date 是当下（自源站生成以来 ≈ 0），但上游已经把它放了 120 秒。
	seedCache(t, store, "/x", &ResponseCache{
		Status: http.StatusOK,
		Body:   []byte("cached"),
		Headers: http.Header{
			"Date": {time.Now().UTC().Format(http.TimeFormat)},
			"Age":  {"120"},
		},
	})

	engine := admissionEngine(store)
	engine.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "fresh") })

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/x", nil))

	if got := recorder.Body.String(); got != "cached" {
		t.Fatalf("body = %q, want cached", got)
	}
	age, err := strconv.Atoi(recorder.Header().Get("Age"))
	if err != nil {
		t.Fatalf("Age = %q, 无法解析: %v", recorder.Header().Get("Age"), err)
	}
	if age < 118 || age > 125 {
		t.Fatalf("Age = %d, want 约 120（只看 Date 会报成 0）", age)
	}
}
