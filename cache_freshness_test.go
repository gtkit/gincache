package gincache

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gtkit/gincache/persist"
)

// ttlRecordingStore 记录实际传给存储的 TTL，用来直接断言 TTL 收敛结果，
// 免去靠 sleep 观察条目过期那种又慢又不稳的做法。
type ttlRecordingStore struct {
	persist.CacheStore

	mu     sync.Mutex
	ttl    time.Duration
	stored bool
}

func (s *ttlRecordingStore) SetWithContext(ctx context.Context, key string, value any, expire time.Duration) error {
	s.mu.Lock()
	s.ttl, s.stored = expire, true
	s.mu.Unlock()
	return s.CacheStore.SetWithContext(ctx, key, value, expire)
}

func (s *ttlRecordingStore) result() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ttl, s.stored
}

// TestResponseFreshnessConstrainsTTL 钉住写入 TTL 受响应声明的新鲜期约束。
//
// handler 声明 max-age=60 而中间件配 10 分钟时，此前会回放 10 分钟。
func TestResponseFreshnessConstrainsTTL(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		configTTL  time.Duration
		setHeader  func(*gin.Context)
		wantStored bool
		wantTTL    time.Duration
	}{
		{
			name:       "声明短于配置以声明为准",
			configTTL:  10 * time.Minute,
			setHeader:  func(c *gin.Context) { c.Header("Cache-Control", "max-age=60") },
			wantStored: true,
			wantTTL:    time.Minute,
		},
		{
			name:       "s-maxage 优先于 max-age",
			configTTL:  10 * time.Minute,
			setHeader:  func(c *gin.Context) { c.Header("Cache-Control", "max-age=600, s-maxage=30") },
			wantStored: true,
			wantTTL:    30 * time.Second,
		},
		{
			name:       "声明长于配置以配置为准",
			configTTL:  time.Minute,
			setHeader:  func(c *gin.Context) { c.Header("Cache-Control", "max-age=600") },
			wantStored: true,
			wantTTL:    time.Minute,
		},
		{
			name:       "未声明沿用配置",
			configTTL:  time.Minute,
			setHeader:  func(*gin.Context) {},
			wantStored: true,
			wantTTL:    time.Minute,
		},
		{
			name:      "Cache-Control 优先于 Expires",
			configTTL: 10 * time.Minute,
			setHeader: func(c *gin.Context) {
				c.Header("Cache-Control", "max-age=60")
				c.Header("Expires", "Thu, 01 Jan 1970 00:00:00 GMT")
			},
			wantStored: true,
			wantTTL:    time.Minute,
		},
		{
			name:       "max-age=0 不进缓存",
			configTTL:  time.Minute,
			setHeader:  func(c *gin.Context) { c.Header("Cache-Control", "max-age=0") },
			wantStored: false,
		},
		{
			name:       "s-maxage=0 不进缓存",
			configTTL:  time.Minute,
			setHeader:  func(c *gin.Context) { c.Header("Cache-Control", "max-age=600, s-maxage=0") },
			wantStored: false,
		},
		{
			name:      "已过期的 Expires 不进缓存",
			configTTL: time.Minute,
			setHeader: func(c *gin.Context) {
				c.Header("Expires", "Thu, 01 Jan 1970 00:00:00 GMT")
			},
			wantStored: false,
		},
		{
			name:       "无法解析的 Expires 按已过期处理",
			configTTL:  time.Minute,
			setHeader:  func(c *gin.Context) { c.Header("Expires", "0") },
			wantStored: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := persist.NewMemoryStore(time.Hour)
			t.Cleanup(func() { _ = inner.Close() })
			store := &ttlRecordingStore{CacheStore: inner}

			engine := gin.New()
			engine.Use(Cache(store, tt.configTTL,
				WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
					return true, Strategy{CacheKey: c.Request.URL.Path}
				}),
			))
			engine.GET("/x", func(c *gin.Context) {
				tt.setHeader(c)
				c.String(http.StatusOK, "ok")
			})

			engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

			ttl, stored := store.result()
			if stored != tt.wantStored {
				t.Fatalf("写入缓存 = %v, want %v", stored, tt.wantStored)
			}
			if stored && ttl != tt.wantTTL {
				t.Fatalf("写入 TTL = %v, want %v", ttl, tt.wantTTL)
			}
		})
	}
}

// TestResponseFreshnessParsing 钉住新鲜期解析的边界行为。
//
// 这些分支要么改变缓存时长、要么决定是否缓存，端到端测不出差别，直接测解析。
func TestResponseFreshnessParsing(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name     string
		header   http.Header
		want     time.Duration
		declared bool
	}{
		{"正常 max-age", http.Header{"Cache-Control": {"max-age=60"}}, time.Minute, true},
		{"s-maxage 优先", http.Header{"Cache-Control": {"max-age=600, s-maxage=30"}}, 30 * time.Second, true},
		{"未声明", http.Header{"Content-Type": {"text/plain"}}, 0, false},

		// 重复指令取最保守值：取首个会让下面两行差出十分钟。
		{"重复 max-age 递增", http.Header{"Cache-Control": {"max-age=0, max-age=600"}}, 0, true},
		{"重复 max-age 递减", http.Header{"Cache-Control": {"max-age=600, max-age=0"}}, 0, true},
		{"重复且相同", http.Header{"Cache-Control": {"max-age=60, max-age=60"}}, time.Minute, true},
		{"跨字段行的重复", http.Header{"Cache-Control": {"max-age=600", "max-age=1"}}, time.Second, true},

		// 非法 delta-seconds 按陈旧处理，而不是当作没出现。
		{"负数", http.Header{"Cache-Control": {"max-age=-1"}}, 0, true},
		{"非数字", http.Header{"Cache-Control": {"max-age=abc"}}, 0, true},
		{"单边引号", http.Header{"Cache-Control": {`max-age="600`}}, 0, true},
		{"成对引号", http.Header{"Cache-Control": {`max-age="600"`}}, 10 * time.Minute, true},

		// 溢出钳到最大值，最终仍会与配置 TTL 取小。
		{"极大值", http.Header{"Cache-Control": {"max-age=99999999999999999"}}, math.MaxInt64, true},
		{"超出 int64", http.Header{"Cache-Control": {"max-age=999999999999999999999999"}}, math.MaxInt64, true},

		// current_age：上游已经消耗掉的部分要扣掉。
		{"Age 用掉一半", http.Header{"Cache-Control": {"max-age=60"}, "Age": {"30"}}, 30 * time.Second, true},
		{"Age 恰好用尽", http.Header{"Cache-Control": {"max-age=60"}, "Age": {"60"}}, 0, true},
		{"Age 超出", http.Header{"Cache-Control": {"max-age=60"}, "Age": {"120"}}, -time.Minute, true},
		{"非法 Age 忽略", http.Header{"Cache-Control": {"max-age=60"}, "Age": {"abc"}}, time.Minute, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, declared := responseFreshness(tt.header)
			if declared != tt.declared {
				t.Fatalf("declared = %v, want %v", declared, tt.declared)
			}
			if declared && got != tt.want {
				t.Fatalf("freshness = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestUpstreamAgeConsumesFreshness 钉住透传上游 Age 的响应不会重获满 TTL。
//
// handler 在做上游代理时会把 Age 一并透传，此时上游的 "max-age=60, Age: 60"
// 已经是陈旧响应，不扣 current_age 就等于又给它续了 60 秒。
func TestUpstreamAgeConsumesFreshness(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		age        string
		wantStored bool
		wantTTL    time.Duration
	}{
		{"用掉一半", "30", true, 30 * time.Second},
		{"恰好用尽", "60", false, 0},
		{"已经超出", "120", false, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := persist.NewMemoryStore(time.Hour)
			t.Cleanup(func() { _ = inner.Close() })
			store := &ttlRecordingStore{CacheStore: inner}

			engine := gin.New()
			engine.Use(Cache(store, 10*time.Minute,
				WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
					return true, Strategy{CacheKey: c.Request.URL.Path}
				}),
			))
			engine.GET("/x", func(c *gin.Context) {
				c.Header("Cache-Control", "max-age=60")
				c.Header("Age", tt.age)
				c.String(http.StatusOK, "ok")
			})

			engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

			ttl, stored := store.result()
			if stored != tt.wantStored {
				t.Fatalf("写入缓存 = %v, want %v", stored, tt.wantStored)
			}
			if stored && ttl != tt.wantTTL {
				t.Fatalf("写入 TTL = %v, want %v", ttl, tt.wantTTL)
			}
		})
	}
}

// TestEtagMatches 钉住 entity-tag 列表的解析。
func TestEtagMatches(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name        string
		ifNoneMatch []string
		etag        string
		want        bool
	}{
		{"精确匹配", []string{`"v1"`}, `"v1"`, true},
		{"弱比较忽略 W/", []string{`W/"v1"`}, `"v1"`, true},
		{"条目侧带 W/", []string{`"v1"`}, `W/"v1"`, true},
		{"星号", []string{`*`}, `"v1"`, true},
		{"不匹配", []string{`"v9"`}, `"v1"`, false},
		{"列表中任一匹配", []string{`"v0", W/"v1"`}, `"v1"`, true},

		// 多个字段行等价于逗号连接的一个列表，Get 只能拿到第一行。
		{"多行任一匹配", []string{`"v0"`, `"v1"`}, `"v1"`, true},
		{"多行都不匹配", []string{`"v0"`, `"v2"`}, `"v1"`, false},

		// 合法的 opaque-tag 允许包含逗号，不能直接按逗号切分。
		{"含逗号的 tag 自匹配", []string{`"a,b"`}, `"a,b"`, true},
		{"含逗号的 tag 不误匹配", []string{`"a,b"`}, `"a"`, false},
		{"含逗号的 tag 在列表中", []string{`"x", "a,b"`}, `"a,b"`, true},

		{"条目无 ETag 时不匹配", []string{`"v1"`}, "", false},
		{"条目无 ETag 时星号仍匹配", []string{`*`}, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := etagMatches(tt.ifNoneMatch, tt.etag); got != tt.want {
				t.Fatalf("etagMatches(%q, %q) = %v, want %v", tt.ifNoneMatch, tt.etag, got, tt.want)
			}
		})
	}
}

// TestStaleEntryNotReplayed 钉住升级前写入的陈旧条目命中后不被回放。
func TestStaleEntryNotReplayed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = store.Close() })

	seedCache(t, store, "/x", &ResponseCache{
		Status:  http.StatusOK,
		Headers: http.Header{"Cache-Control": {"max-age=0"}},
		Body:    []byte("stale"),
	})

	engine := admissionEngine(store)
	engine.GET("/x", func(c *gin.Context) {
		calls.Add(1)
		c.String(http.StatusOK, "fresh")
	})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/x", nil))

	if got := recorder.Body.String(); got != "fresh" {
		t.Fatalf("body = %q, want %q（陈旧条目被回放了）", got, "fresh")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler 被调用 %d 次，want 1", got)
	}
}

// TestConditionalRequestReturnsNotModified 钉住命中时的条件请求处理。
func TestConditionalRequestReturnsNotModified(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const lastModified = "Wed, 21 Oct 2015 07:28:00 GMT"

	tests := []struct {
		name        string
		entryStatus int
		reqHeader   map[string]string
		wantStatus  int
		wantBody    string
	}{
		{
			name:        "If-None-Match 匹配返回 304",
			entryStatus: http.StatusOK,
			reqHeader:   map[string]string{"If-None-Match": `"v1"`},
			wantStatus:  http.StatusNotModified,
		},
		{
			name:        "弱比较忽略 W/ 前缀",
			entryStatus: http.StatusOK,
			reqHeader:   map[string]string{"If-None-Match": `W/"v1"`},
			wantStatus:  http.StatusNotModified,
		},
		{
			name:        "星号匹配",
			entryStatus: http.StatusOK,
			reqHeader:   map[string]string{"If-None-Match": "*"},
			wantStatus:  http.StatusNotModified,
		},
		{
			name:        "列表中任一匹配",
			entryStatus: http.StatusOK,
			reqHeader:   map[string]string{"If-None-Match": `"v0", W/"v1"`},
			wantStatus:  http.StatusNotModified,
		},
		{
			name:        "不匹配时回放完整响应",
			entryStatus: http.StatusOK,
			reqHeader:   map[string]string{"If-None-Match": `"v0"`},
			wantStatus:  http.StatusOK,
			wantBody:    "payload",
		},
		{
			name:        "If-Modified-Since 未变更返回 304",
			entryStatus: http.StatusOK,
			reqHeader:   map[string]string{"If-Modified-Since": lastModified},
			wantStatus:  http.StatusNotModified,
		},
		{
			name:        "If-None-Match 存在时忽略 If-Modified-Since",
			entryStatus: http.StatusOK,
			reqHeader:   map[string]string{"If-None-Match": `"v0"`, "If-Modified-Since": lastModified},
			wantStatus:  http.StatusOK,
			wantBody:    "payload",
		},
		{
			name:        "非 200 条目不产生 304",
			entryStatus: http.StatusAccepted,
			reqHeader:   map[string]string{"If-None-Match": "*"},
			wantStatus:  http.StatusAccepted,
			wantBody:    "payload",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			store := persist.NewMemoryStore(time.Minute)
			t.Cleanup(func() { _ = store.Close() })

			engine := admissionEngine(store)
			engine.GET("/x", func(c *gin.Context) {
				calls.Add(1)
				c.Header("ETag", `"v1"`)
				c.Header("Last-Modified", lastModified)
				c.Data(tt.entryStatus, "text/plain", []byte("payload"))
			})

			// 第一次回源写入缓存。
			engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			for name, value := range tt.reqHeader {
				req.Header.Set(name, value)
			}
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.wantStatus)
			}
			if got := recorder.Body.String(); got != tt.wantBody {
				t.Fatalf("body = %q, want %q", got, tt.wantBody)
			}
			if got := recorder.Header().Get("X-Cache"); got != "HIT" {
				t.Fatalf("X-Cache = %q, want HIT", got)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("handler 被调用 %d 次，want 1（第二次应命中缓存）", got)
			}
		})
	}
}

// TestNotModifiedStripsEntityHeaders 钉住 304 不带实体头。
func TestNotModifiedStripsEntityHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = store.Close() })

	engine := admissionEngine(store)
	engine.GET("/x", func(c *gin.Context) {
		c.Header("ETag", `"v1"`)
		c.Header("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
		c.Data(http.StatusOK, "text/plain", []byte("payload"))
	})

	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("If-None-Match", `"v1"`)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", recorder.Code)
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Encoding", "Last-Modified"} {
		if got := recorder.Header().Get(name); got != "" {
			t.Fatalf("304 响应仍带 %s = %q", name, got)
		}
	}
	if got := recorder.Header().Get("ETag"); got != `"v1"` {
		t.Fatalf("ETag = %q, want %q", got, `"v1"`)
	}
}
