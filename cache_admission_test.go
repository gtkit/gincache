package gincache

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gtkit/gincache/persist"
)

// admissionEngine 构造一个按路径缓存的引擎，缓存键固定为 URL.Path。
func admissionEngine(store persist.CacheStore, opts ...Option) *gin.Engine {
	engine := gin.New()
	all := []Option{
		WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
			return true, Strategy{CacheKey: c.Request.URL.Path}
		}),
	}
	all = append(all, opts...)
	engine.Use(Cache(store, time.Minute, all...))
	return engine
}

// TestDefaultCacheableResponse 钉住内置基线的判定边界。
func TestDefaultCacheableResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name   string
		header http.Header
		want   bool
	}{
		{"空 header 放行", http.Header{}, true},
		{"nil header 放行", nil, true},
		{"Set-Cookie 拒绝", http.Header{"Set-Cookie": {"session=abc"}}, false},
		{"多个 Set-Cookie 拒绝", http.Header{"Set-Cookie": {"a=1", "b=2"}}, false},
		{"no-store 拒绝", http.Header{"Cache-Control": {"no-store"}}, false},
		{"private 拒绝", http.Header{"Cache-Control": {"max-age=60, private"}}, false},
		{"大小写不敏感", http.Header{"Cache-Control": {"No-Store"}}, false},
		{"带空格的指令拒绝", http.Header{"Cache-Control": {"public,   private"}}, false},
		{"private 带参数拒绝", http.Header{"Cache-Control": {`private="X-Foo"`}}, false},
		{"多个 Cache-Control 头拒绝", http.Header{"Cache-Control": {"max-age=60", "no-store"}}, false},
		{"no-cache 放行", http.Header{"Cache-Control": {"no-cache"}}, true},
		{"public 放行", http.Header{"Cache-Control": {"public, max-age=60"}}, true},
		{"指令名不做子串匹配", http.Header{"Cache-Control": {"no-store-hint=1"}}, true},
		{"privately 不误判", http.Header{"Cache-Control": {"privately"}}, true},
		{"Vary 放行", http.Header{"Vary": {"Accept-Encoding"}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DefaultCacheableResponse(http.StatusOK, tt.header); got != tt.want {
				t.Fatalf("DefaultCacheableResponse = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDefaultCacheableResponseComposable 钉住基线可被自定义判据显式组合。
func TestDefaultCacheableResponseComposable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	store := persist.NewMemoryStore(time.Minute)
	engine := admissionEngine(store, WithCacheableResponse(
		func(status int, header http.Header) bool {
			return DefaultCacheableResponse(status, header) && header.Get("X-No-Cache") == ""
		},
	))
	engine.GET("/x", func(c *gin.Context) {
		calls.Add(1)
		c.Header("Set-Cookie", "session=abc")
		c.String(http.StatusOK, "ok")
	})

	for range 2 {
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("handler 被调用 %d 次，want 2（组合进来的基线应挡住 Set-Cookie）", got)
	}
}

// TestCacheableResponseNeverReceivesNilHeader 钉住判据永远拿不到 nil http.Header。
//
// 完全没有响应头的响应（只调 c.Status 的 handler）会让内部 header 为 nil，而
// http.Header.Clone() 对 nil 返回 nil——判据往 nil map 上写一个字段就是 panic，
// 且发生在 singleflight 内会连累所有等待者。
func TestCacheableResponseNeverReceivesNilHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var sawNil atomic.Bool
	store := persist.NewMemoryStore(time.Minute)
	engine := admissionEngine(store, WithCacheableResponse(
		func(_ int, header http.Header) bool {
			if header == nil {
				sawNil.Store(true)
				return false
			}
			// 判据不该改 header，但改了也不能把库打崩。
			header.Set("X-Seen-By-Predicate", "1")
			return true
		},
	))
	engine.GET("/x", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	// 第一次回源写缓存，第二次命中回放——两条路径都会调用判据。
	for range 2 {
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	}

	if sawNil.Load() {
		t.Fatal("判据收到了 nil http.Header")
	}
}

// TestRangeRequestDoesNotReadCache 钉住范围请求不回放已有缓存。
//
// 缓存键不含 Range，回放会把完整响应当成范围响应发出去。
func TestRangeRequestDoesNotReadCache(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	store := persist.NewMemoryStore(time.Minute)
	engine := admissionEngine(store)
	engine.GET("/x", func(c *gin.Context) {
		calls.Add(1)
		c.String(http.StatusOK, "full body")
	})

	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	ranged := httptest.NewRequest(http.MethodGet, "/x", nil)
	ranged.Header.Set("Range", "bytes=0-3")
	engine.ServeHTTP(httptest.NewRecorder(), ranged)

	if got := calls.Load(); got != 2 {
		t.Fatalf("handler 被调用 %d 次，want 2（带 Range 的请求不应命中缓存）", got)
	}
}

// TestRangeRequestDoesNotWriteCache 钉住范围请求的响应不进缓存。
func TestRangeRequestDoesNotWriteCache(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	store := persist.NewMemoryStore(time.Minute)
	engine := admissionEngine(store)
	engine.GET("/x", func(c *gin.Context) {
		calls.Add(1)
		c.String(http.StatusOK, "full body")
	})

	ranged := httptest.NewRequest(http.MethodGet, "/x", nil)
	ranged.Header.Set("Range", "bytes=0-3")
	engine.ServeHTTP(httptest.NewRecorder(), ranged)

	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	if got := calls.Load(); got != 2 {
		t.Fatalf("handler 被调用 %d 次，want 2（带 Range 的响应不应写入缓存）", got)
	}
}

// TestPartialContentNotCacheableByDefault 钉住默认状态码集合排除 206。
func TestPartialContentNotCacheableByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name      string
		status    int
		opts      []Option
		wantCalls int32
	}{
		{"206 默认不缓存", http.StatusPartialContent, nil, 2},
		{"200 默认缓存", http.StatusOK, nil, 1},
		{
			name:      "显式配置 206 时缓存",
			status:    http.StatusPartialContent,
			opts:      []Option{WithCacheableStatusCodes(http.StatusPartialContent)},
			wantCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			store := persist.NewMemoryStore(time.Minute)
			engine := admissionEngine(store, tt.opts...)
			engine.GET("/x", func(c *gin.Context) {
				calls.Add(1)
				c.Data(tt.status, "text/plain", []byte("part"))
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

// TestHandlerWithoutWriteIsNotCached 钉住 handler 什么都没写时不产生缓存条目。
//
// 此时 Status 和 Body 都是包装器编出来的默认值，缓存它等于用一个空 200
// 顶掉后续所有请求。
func TestHandlerWithoutWriteIsNotCached(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	store := persist.NewMemoryStore(time.Minute)
	engine := admissionEngine(store)
	engine.GET("/x", func(*gin.Context) { calls.Add(1) })

	for range 2 {
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("handler 被调用 %d 次，want 2（未写出任何内容的响应不应进缓存）", got)
	}
}

// TestHijackedResponseIsNotCached 钉住 Hijack 接管连接后不产生缓存条目。
//
// Hijack 从内嵌的 gin.ResponseWriter 提升上去，包装器完全观察不到写入：
// 若照旧缓存，一个空 200 会顶掉后续所有握手。
func TestHijackedResponseIsNotCached(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	store := persist.NewMemoryStore(time.Minute)
	engine := admissionEngine(store)
	engine.GET("/ws", func(c *gin.Context) {
		calls.Add(1)
		conn, buf, err := c.Writer.Hijack()
		if err != nil {
			t.Errorf("Hijack 失败: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 8\r\n\r\nhijacked")
		_ = buf.Flush()
	})

	srv := httptest.NewServer(engine)
	defer srv.Close()

	for range 2 {
		resp, err := http.Get(srv.URL + "/ws")
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != "hijacked" {
			t.Fatalf("body = %q, want %q", body, "hijacked")
		}
	}

	if got := calls.Load(); got != 2 {
		t.Fatalf("handler 被调用 %d 次，want 2（Hijack 后的响应不应进缓存）", got)
	}
}

// TestHopByHopHeadersStrippedOnWrite 钉住连接级 header 不进缓存。
func TestHopByHopHeadersStrippedOnWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := persist.NewMemoryStore(time.Minute)
	engine := admissionEngine(store)
	engine.GET("/x", func(c *gin.Context) {
		c.Header("Connection", "keep-alive, X-Custom-Hop")
		c.Header("X-Custom-Hop", "v")
		c.Header("Keep-Alive", "timeout=5")
		// TE 的规范形式是 "Te"，清单里存的也必须是规范形式才删得掉。
		c.Header("TE", "trailers")
		c.Header("Transfer-Encoding", "chunked")
		c.Header("Content-Type", "text/plain")
		c.String(http.StatusOK, "ok")
	})

	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	var cached ResponseCache
	if err := store.Get("/x", &cached); err != nil {
		t.Fatalf("缓存未写入: %v", err)
	}

	for _, name := range []string{"Connection", "Keep-Alive", "TE", "Transfer-Encoding", "X-Custom-Hop"} {
		if got := cached.Headers.Get(name); got != "" {
			t.Fatalf("缓存里仍有连接级 header %s = %q", name, got)
		}
		// 旧式单值视图的键是规范形式，按规范键查才不会查了个寂寞。
		if got := cached.Header[http.CanonicalHeaderKey(name)]; got != "" {
			t.Fatalf("旧式单值视图里仍有连接级 header %s = %q", name, got)
		}
	}
	if got := cached.Headers.Get("Content-Type"); got == "" {
		t.Fatal("Content-Type 被误删")
	}
}

// TestHopByHopHeadersStrippedOnReplay 钉住历史条目回放时也剔除连接级 header。
//
// 写入侧过滤只管新数据；已经写进 store 的旧条目只能在回放侧补掉。
func TestHopByHopHeadersStrippedOnReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := persist.NewMemoryStore(time.Minute)
	seedCache(t, store, "/x", &ResponseCache{
		Status: http.StatusOK,
		Headers: http.Header{
			"Content-Type": {"text/plain"},
			"Connection":   {"keep-alive"},
		},
		Body: []byte("cached"),
	})

	engine := admissionEngine(store)
	engine.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "fresh") })

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/x", nil))

	if got := recorder.Body.String(); got != "cached" {
		t.Fatalf("body = %q, want %q（合规条目应正常回放）", got, "cached")
	}
	if got := recorder.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("X-Cache = %q, want HIT", got)
	}
	if got := recorder.Header().Get("Connection"); got != "" {
		t.Fatalf("回放写出了连接级 header Connection = %q", got)
	}
}

// TestLegacyEntryRecheckedBeforeReplay 钉住旧判据下写入的条目命中后不被回放。
//
// 这是升级即生效的关键：不要求调用方轮换 key 前缀或清空 store。
func TestLegacyEntryRecheckedBeforeReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name  string
		entry *ResponseCache
	}{
		{
			name: "完整 header 视图含 Set-Cookie",
			entry: &ResponseCache{
				Status:  http.StatusOK,
				Headers: http.Header{"Set-Cookie": {"session=leaked"}},
				Body:    []byte("cached"),
			},
		},
		{
			name: "仅旧式单值视图含 Set-Cookie",
			entry: &ResponseCache{
				Status: http.StatusOK,
				Header: map[string]string{"Set-Cookie": "session=leaked"},
				Body:   []byte("cached"),
			},
		},
		{
			name: "历史 206 条目",
			entry: &ResponseCache{
				Status:  http.StatusPartialContent,
				Headers: http.Header{"Content-Range": {"bytes 0-3/9"}},
				Body:    []byte("cach"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := persist.NewMemoryStore(time.Minute)
			seedCache(t, store, "/x", tt.entry)

			engine := admissionEngine(store)
			engine.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "fresh") })

			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/x", nil))

			if got := recorder.Body.String(); got != "fresh" {
				t.Fatalf("body = %q, want %q（不合规条目不应回放）", got, "fresh")
			}
			if got := recorder.Header().Get("Set-Cookie"); got != "" {
				t.Fatalf("回放泄漏了 Set-Cookie = %q", got)
			}
		})
	}
}

// TestSingleFlightDoesNotShareUncacheableResponse 钉住不可共享的响应不发给并发等待者。
//
// singleflight 把 leader 的响应直接交给等待者，这条路径绕过 store——leader 的
// Set-Cookie 会被原样发给别人，和缓存泄漏是同一类会话串号。
func TestSingleFlightDoesNotShareUncacheableResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})

	store := persist.NewMemoryStore(time.Minute)
	engine := admissionEngine(store)
	engine.GET("/x", func(c *gin.Context) {
		n := calls.Add(1)
		if n == 1 {
			close(entered)
			<-release
		}
		c.Header("Set-Cookie", fmt.Sprintf("session=user%d", n))
		c.String(http.StatusOK, "user%d", n)
	})

	bodies := make([]string, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/x", nil))
		bodies[0] = recorder.Body.String()
	})

	<-entered
	wg.Go(func() {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/x", nil))
		bodies[1] = recorder.Body.String()
	})

	// 给第二个请求足够时间进入 singleflight 的等待队列。
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if bodies[0] == bodies[1] {
		t.Fatalf("两个请求拿到同一份响应 %q：leader 的 Set-Cookie 被发给了等待者", bodies[0])
	}
}

// TestSingleFlightSharesCacheableResponse 钉住合规响应仍然正常共享。
func TestSingleFlightSharesCacheableResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})

	store := persist.NewMemoryStore(time.Minute)
	engine := admissionEngine(store)
	engine.GET("/x", func(c *gin.Context) {
		n := calls.Add(1)
		if n == 1 {
			close(entered)
			<-release
		}
		c.String(http.StatusOK, "user%d", n)
	})

	bodies := make([]string, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/x", nil))
		bodies[0] = recorder.Body.String()
	})

	<-entered
	wg.Go(func() {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/x", nil))
		bodies[1] = recorder.Body.String()
	})

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("handler 被调用 %d 次，want 1（合规响应应被等待者共享）", got)
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("两个请求拿到不同响应 %q / %q，want 相同", bodies[0], bodies[1])
	}
}

// ExampleDefaultCacheableResponse 展示内置基线的判定结果。
func ExampleDefaultCacheableResponse() {
	fmt.Println(DefaultCacheableResponse(http.StatusOK, http.Header{"Set-Cookie": {"session=abc"}}))
	fmt.Println(DefaultCacheableResponse(http.StatusOK, http.Header{"Cache-Control": {"max-age=60, private"}}))
	fmt.Println(DefaultCacheableResponse(http.StatusOK, http.Header{"Cache-Control": {"public, max-age=60"}}))
	// Output:
	// false
	// false
	// true
}

// ExampleWithCacheableResponse 展示如何在自定义判据里保留内置基线。
//
// 自定义判据替换基线而不是叠加，需要基线的必须显式组合。
func ExampleWithCacheableResponse() {
	gin.SetMode(gin.TestMode)

	predicate := func(status int, header http.Header) bool {
		return DefaultCacheableResponse(status, header) && header.Get("X-No-Cache") == ""
	}

	engine := gin.New()
	engine.Use(Cache(persist.NewMemoryStore(time.Minute), time.Minute,
		WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
			return true, Strategy{CacheKey: c.Request.URL.Path}
		}),
		WithCacheableResponse(predicate),
	))

	fmt.Println(predicate(http.StatusOK, http.Header{"Set-Cookie": {"session=abc"}}))
	fmt.Println(predicate(http.StatusOK, http.Header{"X-No-Cache": {"1"}}))
	fmt.Println(predicate(http.StatusOK, http.Header{"Cache-Control": {"public"}}))
	// Output:
	// false
	// false
	// true
}

// seedCache 直接向 store 写入一个缓存条目，用于构造历史数据。
func seedCache(t *testing.T, store persist.CacheStore, key string, entry *ResponseCache) {
	t.Helper()
	if err := store.Set(key, entry, time.Minute); err != nil {
		t.Fatalf("写入历史缓存条目失败: %v", err)
	}
}
