package gincache

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
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
			// current_age 含 response_delay（handler 全程耗时），写入的 TTL 会比
			// 声明值少掉那一点点，用区间而不是精确值断言。
			if stored && (ttl > tt.wantTTL || ttl < tt.wantTTL-time.Second) {
				t.Fatalf("写入 TTL = %v, want 约 %v", ttl, tt.wantTTL)
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

		// 取值冲突的重复按陈旧处理；取值相同的重复不算冲突。
		{"冲突重复 递增", http.Header{"Cache-Control": {"max-age=0, max-age=600"}}, 0, true},
		{"冲突重复 递减", http.Header{"Cache-Control": {"max-age=600, max-age=0"}}, 0, true},
		{"冲突重复 都是正数", http.Header{"Cache-Control": {"max-age=600, max-age=60"}}, 0, true},
		{"重复且相同", http.Header{"Cache-Control": {"max-age=60, max-age=60"}}, time.Minute, true},
		{"跨字段行的冲突重复", http.Header{"Cache-Control": {"max-age=600", "max-age=1"}}, 0, true},
		{"s-maxage 与 max-age 不算冲突", http.Header{"Cache-Control": {"max-age=600, s-maxage=30"}}, 30 * time.Second, true},

		// 非法 delta-seconds 按陈旧处理，而不是当作没出现。
		// delta-seconds 的语法是 1*DIGIT，符号位不合法。
		{"负数", http.Header{"Cache-Control": {"max-age=-1"}}, 0, true},
		{"正号", http.Header{"Cache-Control": {"max-age=+600"}}, 0, true},
		{"非数字", http.Header{"Cache-Control": {"max-age=abc"}}, 0, true},
		{"空值", http.Header{"Cache-Control": {"max-age="}}, 0, true},
		{"单边引号", http.Header{"Cache-Control": {`max-age="600`}}, 0, true},
		{"成对引号", http.Header{"Cache-Control": {`max-age="600"`}}, 10 * time.Minute, true},

		// 溢出钳到最大值，最终仍会与配置 TTL 取小。
		{"极大值", http.Header{"Cache-Control": {"max-age=99999999999999999"}}, math.MaxInt64, true},
		{"超出 int64", http.Header{"Cache-Control": {"max-age=999999999999999999999999"}}, math.MaxInt64, true},

		// current_age：上游已经消耗掉的部分要扣掉。
		{"Age 用掉一半", http.Header{"Cache-Control": {"max-age=60"}, "Age": {"30"}}, 30 * time.Second, true},
		{"Age 恰好用尽", http.Header{"Cache-Control": {"max-age=60"}, "Age": {"60"}}, 0, true},
		{"Age 超出", http.Header{"Cache-Control": {"max-age=60"}, "Age": {"120"}}, 0, true},
		{"非法 Age 忽略", http.Header{"Cache-Control": {"max-age=60"}, "Age": {"abc"}}, time.Minute, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, declared := responseFreshness(tt.header, 0)
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
			// current_age 现在还含 response_delay（handler 全程耗时），写入的 TTL
			// 会比声明值少掉那一点点，用区间而不是精确值断言。
			if stored && (ttl > tt.wantTTL || ttl < tt.wantTTL-time.Second) {
				t.Fatalf("写入 TTL = %v, want 约 %v", ttl, tt.wantTTL)
			}
		})
	}
}

// TestZeroConfigTTLKeepsStoreDefault 钉住 defaultExpire=0 的"交由存储决定"不被顶开。
//
// 声明的新鲜期不该反过来覆盖调用方给出的这个契约：一个完全合法的
// max-age=31536000 会把存储的默认 TTL 变成一年。
func TestZeroConfigTTLKeepsStoreDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		cacheCtrl  string
		wantStored bool
	}{
		{"超长 max-age 不顶开存储默认", "max-age=31536000", true},
		{"短 max-age 也不顶开", "max-age=1", true},
		{"陈旧仍然被拒", "max-age=0", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := persist.NewMemoryStore(time.Minute)
			t.Cleanup(func() { _ = inner.Close() })
			store := &ttlRecordingStore{CacheStore: inner}

			engine := gin.New()
			// defaultExpire = 0：交由存储决定。
			engine.Use(Cache(store, 0,
				WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
					return true, Strategy{CacheKey: c.Request.URL.Path}
				}),
			))
			engine.GET("/x", func(c *gin.Context) {
				c.Header("Cache-Control", tt.cacheCtrl)
				c.String(http.StatusOK, "ok")
			})

			engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

			ttl, stored := store.result()
			if stored != tt.wantStored {
				t.Fatalf("写入缓存 = %v, want %v", stored, tt.wantStored)
			}
			if stored && ttl != 0 {
				t.Fatalf("写入 TTL = %v, want 0（应原样交给存储决定）", ttl)
			}
		})
	}
}

// TestReplayEmitsAge 钉住命中回放时写出 Age。
//
// 只扣本级 TTL 不够：下游的 CDN 或浏览器看到偏小的 Age 会把这份响应再多留一会儿。
func TestReplayEmitsAge(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("本包写入的条目带 Age", func(t *testing.T) {
		store := persist.NewMemoryStore(time.Minute)
		t.Cleanup(func() { _ = store.Close() })

		engine := admissionEngine(store)
		engine.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

		replay := httptest.NewRecorder()
		engine.ServeHTTP(replay, httptest.NewRequest(http.MethodGet, "/x", nil))

		if got := replay.Header().Get("X-Cache"); got != "HIT" {
			t.Fatalf("X-Cache = %q, want HIT", got)
		}
		if got := replay.Header().Get("Age"); got != "0" {
			t.Fatalf("Age = %q, want \"0\"（刚写入的条目年龄为 0 秒）", got)
		}
	})

	t.Run("上游 Age 与驻留时长叠加", func(t *testing.T) {
		store := persist.NewMemoryStore(time.Minute)
		t.Cleanup(func() { _ = store.Close() })

		// 构造一个 100 秒前写入、入库时已有 20 秒年龄的条目。
		seedCache(t, store, "/x", &ResponseCache{
			Status:       http.StatusOK,
			Body:         []byte("cached"),
			ResponseTime: time.Now().Add(-100 * time.Second).UnixNano(),
			InitialAge:   int64(20 * time.Second),
		})

		engine := admissionEngine(store)
		engine.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "fresh") })

		replay := httptest.NewRecorder()
		engine.ServeHTTP(replay, httptest.NewRequest(http.MethodGet, "/x", nil))

		if got := replay.Body.String(); got != "cached" {
			t.Fatalf("body = %q, want cached", got)
		}
		age, err := strconv.Atoi(replay.Header().Get("Age"))
		if err != nil {
			t.Fatalf("Age = %q, 无法解析: %v", replay.Header().Get("Age"), err)
		}
		if age < 118 || age > 125 {
			t.Fatalf("Age = %d, want 约 120（20 秒入库年龄 + 100 秒驻留）", age)
		}
	})

	t.Run("无法估算年龄的条目不被回放", func(t *testing.T) {
		store := persist.NewMemoryStore(time.Minute)
		t.Cleanup(func() { _ = store.Close() })

		// 本包此前写入的条目既没有 ResponseTime 也没有 Date。
		seedCache(t, store, "/x", &ResponseCache{
			Status:  http.StatusOK,
			Headers: http.Header{"Content-Type": {"text/plain"}},
			Body:    []byte("cached"),
		})

		engine := admissionEngine(store)
		engine.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "fresh") })

		replay := httptest.NewRecorder()
		engine.ServeHTTP(replay, httptest.NewRequest(http.MethodGet, "/x", nil))

		if got := replay.Body.String(); got != "fresh" {
			t.Fatalf("body = %q, want fresh（估不出年龄的条目不该被回放）", got)
		}
	})

	t.Run("Date 存在时据此估算", func(t *testing.T) {
		store := persist.NewMemoryStore(time.Minute)
		t.Cleanup(func() { _ = store.Close() })

		seedCache(t, store, "/x", &ResponseCache{
			Status:  http.StatusOK,
			Headers: http.Header{"Date": {time.Now().Add(-90 * time.Second).UTC().Format(http.TimeFormat)}},
			Body:    []byte("cached"),
		})

		engine := admissionEngine(store)
		engine.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "fresh") })

		replay := httptest.NewRecorder()
		engine.ServeHTTP(replay, httptest.NewRequest(http.MethodGet, "/x", nil))

		age, err := strconv.Atoi(replay.Header().Get("Age"))
		if err != nil {
			t.Fatalf("Age = %q, 无法解析: %v", replay.Header().Get("Age"), err)
		}
		if age < 88 || age > 95 {
			t.Fatalf("Age = %d, want 约 90（now - Date）", age)
		}
	})
}

// TestConditionalHeaderEdgeCases 钉住条件请求头的存在性与重复处理。
func TestConditionalHeaderEdgeCases(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const lastModified = "Wed, 21 Oct 2015 07:28:00 GMT"

	newEngine := func(t *testing.T) *gin.Engine {
		t.Helper()
		store := persist.NewMemoryStore(time.Minute)
		t.Cleanup(func() { _ = store.Close() })

		engine := admissionEngine(store)
		engine.GET("/x", func(c *gin.Context) {
			c.Header("ETag", `"v1"`)
			c.Header("Last-Modified", lastModified)
			c.Data(http.StatusOK, "text/plain", []byte("payload"))
		})
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
		return engine
	}

	t.Run("空 If-None-Match 压制 If-Modified-Since", func(t *testing.T) {
		engine := newEngine(t)

		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("If-None-Match", "")
		req.Header.Set("If-Modified-Since", lastModified)

		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200：If-None-Match 存在就该压制 If-Modified-Since", recorder.Code)
		}
	})

	t.Run("重复的 If-Modified-Since 被忽略", func(t *testing.T) {
		engine := newEngine(t)

		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Add("If-Modified-Since", lastModified)
		req.Header.Add("If-Modified-Since", "garbage")

		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200：重复的 If-Modified-Since 属畸形请求，应整体忽略", recorder.Code)
		}
	})

	t.Run("单个 If-Modified-Since 正常生效", func(t *testing.T) {
		engine := newEngine(t)

		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("If-Modified-Since", lastModified)

		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want 304", recorder.Code)
		}
	})
}

// TestFreshnessSaturationDoesNotWrap 钉住饱和值相减不会整数回绕。
//
// Time.Sub 超过约 292 年就会顶到 MinInt64 / MaxInt64，两个饱和值直接相减时
// MinInt64 - MaxInt64 恰好绕成 +1ns——几百年前就过期的响应会被判成还新鲜。
func TestFreshnessSaturationDoesNotWrap(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name   string
		header http.Header
	}{
		{
			name: "Expires 与 Date 都极其久远",
			header: http.Header{
				"Date":    {"Mon, 02 Jan 1200 15:04:05 GMT"},
				"Expires": {"Mon, 02 Jan 0800 15:04:05 GMT"},
			},
		},
		{
			name: "Date 极久远而 Expires 稍晚",
			header: http.Header{
				"Date":    {"Mon, 02 Jan 1200 15:04:05 GMT"},
				"Expires": {"Mon, 02 Jan 1300 15:04:05 GMT"},
			},
		},
		{
			name: "极大 max-age 配极久远的 Date",
			header: http.Header{
				"Cache-Control": {"max-age=99999999999999999"},
				"Date":          {"Mon, 02 Jan 1200 15:04:05 GMT"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, declared := responseFreshness(tt.header, 0)
			if !declared {
				t.Fatal("declared = false, want true")
			}
			if got > 0 {
				t.Fatalf("freshness = %v, want <= 0（饱和值相减回绕了）", got)
			}
		})
	}
}

// TestReplayAgeDoesNotOverflow 钉住入库年龄与驻留时长相加不会回绕。
func TestReplayAgeDoesNotOverflow(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = store.Close() })

	seedCache(t, store, "/x", &ResponseCache{
		Status:       http.StatusOK,
		Body:         []byte("cached"),
		ResponseTime: time.Now().Add(-10 * time.Second).UnixNano(),
		InitialAge:   math.MaxInt64,
	})

	engine := admissionEngine(store)
	engine.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "fresh") })

	replay := httptest.NewRecorder()
	engine.ServeHTTP(replay, httptest.NewRequest(http.MethodGet, "/x", nil))

	age, err := strconv.ParseInt(replay.Header().Get("Age"), 10, 64)
	if err != nil {
		t.Fatalf("Age = %q, 无法解析: %v", replay.Header().Get("Age"), err)
	}
	if age <= 0 {
		t.Fatalf("Age = %d, want 极大正数（相加回绕成负数后被 max 吃掉了）", age)
	}
}

// TestUnknownAgeEntryTreatedAsMiss 钉住估算不出年龄的条目按未命中处理。
//
// RFC 要求复用存储的响应时给出当前 Age，给不出就不该复用。这类条目只可能来自
// 本版本之前的写入：handler 重新产生响应并带上入库时刻回填，因此每个 key 只需
// 一次回源，而且被 singleflight 合并。
func TestUnknownAgeEntryTreatedAsMiss(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = store.Close() })

	// 本包 v1.3.0 之前写入的条目：有上游透传的 Age，但既无 Date 也无 StoredAt。
	seedCache(t, store, "/x", &ResponseCache{
		Status:  http.StatusOK,
		Headers: http.Header{"Age": {"100"}, "Content-Type": {"text/plain"}},
		Body:    []byte("cached"),
	})

	engine := admissionEngine(store)
	engine.GET("/x", func(c *gin.Context) {
		calls.Add(1)
		c.String(http.StatusOK, "fresh")
	})

	replay := httptest.NewRecorder()
	engine.ServeHTTP(replay, httptest.NewRequest(http.MethodGet, "/x", nil))

	if got := replay.Body.String(); got != "fresh" {
		t.Fatalf("body = %q, want fresh（估不出年龄的条目不该被回放）", got)
	}
	if got := replay.Header().Get("Age"); got != "" {
		t.Fatalf("Age = %q, want 空（本次是 handler 的新响应，不该带 Age）", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler 被调用 %d 次，want 1", got)
	}

	// 回填之后带上了入库时刻，下一次就能正常回放并给出 Age。
	second := httptest.NewRecorder()
	engine.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/x", nil))
	if got := second.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("X-Cache = %q, want HIT（回填后应可正常命中）", got)
	}
	if got := second.Header().Get("Age"); got != "0" {
		t.Fatalf("Age = %q, want \"0\"", got)
	}
}

// TestNonCanonicalRequestHeaders 钉住请求头判断不因大小写而失效。
//
// 真实网络流量经 net/http 解析后键必为规范形式，但程序化构造的请求或前置
// 中间件可能留下非规范键。
func TestNonCanonicalRequestHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newEngine := func(t *testing.T, calls *atomic.Int32) *gin.Engine {
		t.Helper()
		store := persist.NewMemoryStore(time.Minute)
		t.Cleanup(func() { _ = store.Close() })

		engine := admissionEngine(store)
		engine.GET("/x", func(c *gin.Context) {
			calls.Add(1)
			c.Header("ETag", `"v1"`)
			c.Header("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
			c.Data(http.StatusOK, "text/plain", []byte("full body"))
		})
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
		return engine
	}

	t.Run("小写 range 不绕过范围请求门禁", func(t *testing.T) {
		var calls atomic.Int32
		engine := newEngine(t, &calls)

		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header["range"] = []string{"bytes=0-3"}
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)

		if got := recorder.Header().Get("X-Cache"); got == "HIT" {
			t.Fatal("带 range 的请求命中了缓存：门禁被非规范键绕过")
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("handler 被调用 %d 次，want 2", got)
		}
	})

	t.Run("小写 if-none-match 仍压制 If-Modified-Since", func(t *testing.T) {
		var calls atomic.Int32
		engine := newEngine(t, &calls)

		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header["if-none-match"] = []string{`"nope"`}
		req.Header.Set("If-Modified-Since", "Wed, 21 Oct 2015 07:28:00 GMT")
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200：非规范键的 If-None-Match 应被识别并压制 IMS", recorder.Code)
		}
	})

	t.Run("小写 if-none-match 匹配时返回 304", func(t *testing.T) {
		var calls atomic.Int32
		engine := newEngine(t, &calls)

		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header["if-none-match"] = []string{`"v1"`}
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want 304", recorder.Code)
		}
	})

	t.Run("小写 if-modified-since 生效", func(t *testing.T) {
		var calls atomic.Int32
		engine := newEngine(t, &calls)

		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header["if-modified-since"] = []string{"Wed, 21 Oct 2015 07:28:00 GMT"}
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, req)

		if recorder.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want 304", recorder.Code)
		}
	})
}

// TestDuplicateCaseHeadersAreDeterministic 钉住大小写不同的同名键不产生随机结果。
//
// 它们在 HTTP 语义上是同一个字段。赋值式收集会让后遍历到的覆盖前一个，而 Go 的
// map 遍历顺序是随机的——同一份输入可能算出不同结果。
func TestDuplicateCaseHeadersAreDeterministic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("新鲜期解析", func(t *testing.T) {
		results := map[time.Duration]int{}
		for range 50 {
			header := http.Header{}
			header["Cache-Control"] = []string{"max-age=600"}
			header["cache-control"] = []string{"max-age=1"}
			got, _ := responseFreshness(header, 0)
			results[got]++
		}
		if len(results) != 1 {
			t.Fatalf("50 次得到 %d 种结果 %v，want 1 种", len(results), results)
		}
		// 两个取值冲突的 max-age 合并后按陈旧处理。
		if _, ok := results[0]; !ok {
			t.Fatalf("结果 = %v, want 0（冲突的重复应判为陈旧）", results)
		}
	})

	t.Run("条件请求头", func(t *testing.T) {
		store := persist.NewMemoryStore(time.Minute)
		t.Cleanup(func() { _ = store.Close() })

		engine := admissionEngine(store)
		engine.GET("/x", func(c *gin.Context) {
			c.Header("ETag", `"v1"`)
			c.Data(http.StatusOK, "text/plain", []byte("payload"))
		})
		engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

		results := map[int]int{}
		for range 50 {
			request := httptest.NewRequest(http.MethodGet, "/x", nil)
			request.Header["If-None-Match"] = []string{`"v1"`}
			request.Header["if-none-match"] = []string{`"nope"`}
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, request)
			results[recorder.Code]++
		}
		if len(results) != 1 {
			t.Fatalf("50 次得到 %d 种状态码 %v，want 1 种", len(results), results)
		}
		// 两个字段行合并成一个列表，其中有一个匹配就该是 304。
		if _, ok := results[http.StatusNotModified]; !ok {
			t.Fatalf("状态码 = %v, want 304（合并后的列表里有匹配项）", results)
		}
	})
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

// TestStaleEntryNotReplayedAfterResidence 钉住条目活过自己的新鲜期后不再被回放。
//
// 只靠存储 TTL 兜不住：defaultExpire 传 0 时 TTL 由存储决定，存储默认值比声明的
// 新鲜期长，条目就会活过自己的新鲜期还被当成新鲜的回放。
func TestStaleEntryNotReplayedAfterResidence(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32
	// 存储默认 TTL 一分钟，远长于响应声明的 1 秒。
	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = store.Close() })

	engine := gin.New()
	engine.Use(Cache(store, 0, // 0：交由存储决定 TTL
		WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
			return true, Strategy{CacheKey: c.Request.URL.Path}
		}),
	))
	engine.GET("/x", func(c *gin.Context) {
		calls.Add(1)
		c.Header("Cache-Control", "max-age=1")
		c.String(http.StatusOK, "ok")
	})

	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	// 刚写入时应当正常命中。
	fresh := httptest.NewRecorder()
	engine.ServeHTTP(fresh, httptest.NewRequest(http.MethodGet, "/x", nil))
	if got := fresh.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("X-Cache = %q, want HIT（新鲜期内应命中）", got)
	}

	time.Sleep(1200 * time.Millisecond)

	stale := httptest.NewRecorder()
	engine.ServeHTTP(stale, httptest.NewRequest(http.MethodGet, "/x", nil))
	if got := stale.Header().Get("X-Cache"); got == "HIT" {
		t.Fatalf("超过 max-age=1 后仍然命中，Age=%q", stale.Header().Get("Age"))
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler 被调用 %d 次，want 2", got)
	}
}

// TestInitialAgeRecordedOnWrite 钉住写入时把 corrected_initial_age 记进条目。
//
// 回放时的当前年龄就是它加上驻留时长；写入时的 response_delay 与上游的 Age
// 都折进这一个数，回放侧不必再解析条目里那个可能畸形的 Age 头。
func TestInitialAgeRecordedOnWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = store.Close() })

	engine := admissionEngine(store)
	engine.GET("/x", func(c *gin.Context) {
		// 畸形的列表形式，取第一个成员。
		c.Header("Age", "20, 40")
		c.String(http.StatusOK, "ok")
	})

	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

	var cached ResponseCache
	if err := store.Get("/x", &cached); err != nil {
		t.Fatalf("缓存未写入: %v", err)
	}
	// 纳秒精度：20 秒的上游年龄加上 handler 的一点点耗时。
	if got := time.Duration(cached.InitialAge); got < 20*time.Second || got > 21*time.Second {
		t.Fatalf("InitialAge = %v, want 约 20s（取列表首个成员）", got)
	}
	if cached.ResponseTime == 0 {
		t.Fatal("ResponseTime 未记录")
	}
}

// TestMultipleAgeFieldLines 钉住多个 Age 字段行不被整个忽略。
func TestMultipleAgeFieldLines(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 取最大值：既确定又保守，整个忽略会把年龄算成 0（fail-open）。
	got, declared := responseFreshness(
		http.Header{"Cache-Control": {"max-age=60"}, "Age": {"30", "40"}}, 0)
	if !declared {
		t.Fatal("declared = false, want true")
	}
	if got != 20*time.Second {
		t.Fatalf("freshness = %v, want 20s（60 减去最大的 Age 40）", got)
	}
}

// TestResponseDelayCountedWithoutAge 钉住没有 Age 头时 response_delay 照样计入年龄。
func TestResponseDelayCountedWithoutAge(t *testing.T) {
	gin.SetMode(gin.TestMode)

	got, declared := responseFreshness(http.Header{"Cache-Control": {"max-age=60"}}, 90*time.Second)
	if !declared {
		t.Fatal("declared = false, want true")
	}
	if got != 0 {
		t.Fatalf("freshness = %v, want 0（90 秒的回源耗时已经耗尽 60 秒新鲜期）", got)
	}
}
