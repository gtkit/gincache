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

// TestSupersededLeaderDoesNotOverwriteNewerEntry 钉住被超时释放的 leader 不再覆盖新缓存。
//
// Forget 只解除 key 与该 flight 的关联，不取消这条 flight。被释放之后新 leader
// 可能先跑完并写入较新的响应，而旧 leader 随后完成时若照样写缓存，就把新响应盖成
// 了旧的。响应自带的年龄机制兜不住：没有 Cache-Control 时新鲜期未声明，回放不会
// 被拒。
//
// 时间线（forgetTimeout = 200ms）：
//
//	t=0    请求 A 成为 leader，进入 handler 后阻塞；它最终返回 "v1"
//	t=200  定时器触发：置位淘汰标记并 Forget
//	t=300  请求 B 进入，成为新 leader，立即返回 "v2" 并写入缓存
//	t=400  放行 A，A 返回 "v1"——它已被淘汰，不得写缓存
//	t=500  请求 C 读缓存：必须拿到 "v2"
func TestSupersededLeaderDoesNotOverwriteNewerEntry(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const forgetTimeout = 200 * time.Millisecond

	var calls atomic.Int32
	release := make(chan struct{})

	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = store.Close() })

	engine := gin.New()
	engine.Use(Cache(store, time.Minute,
		WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
			return true, Strategy{CacheKey: c.Request.URL.Path}
		}),
		WithSingleFlightForgetTimeout(forgetTimeout),
	))
	engine.GET("/x", func(c *gin.Context) {
		switch calls.Add(1) {
		case 1:
			<-release // 旧 leader：慢
			c.String(http.StatusOK, "v1")
		default:
			c.String(http.StatusOK, "v2") // 新 leader：快
		}
	})

	serve := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		return rec
	}

	var aBody string
	var wg sync.WaitGroup
	wg.Go(func() { aBody = serve().Body.String() })

	time.Sleep(300 * time.Millisecond) // 越过淘汰点

	if got := serve().Body.String(); got != "v2" {
		t.Fatalf("请求 B（新 leader）body = %q, want \"v2\"", got)
	}

	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	// 被淘汰的 leader 仍要把响应交给自己的调用方——那是它自己的请求。
	if aBody != "v1" {
		t.Fatalf("请求 A（被淘汰的 leader）body = %q, want \"v1\"", aBody)
	}

	time.Sleep(50 * time.Millisecond)

	cRec := serve()
	if got := cRec.Body.String(); got != "v2" {
		t.Fatalf("请求 C body = %q, want \"v2\"——被淘汰的 leader 覆盖了新响应", got)
	}
	if got := cRec.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("请求 C X-Cache = %q, want HIT", got)
	}
}

// TestSupersessionBetweenCheckAndWriteIsCaught 钉住"检查通过之后才被淘汰"也拦得住。
//
// 只在写入前查一次不够：定时器可以在查过之后触发，新 leader 随即写入更新的响应，
// 而这份较旧的结果最后落地就把它盖掉。检查与写入放进同一把分片锁之后，新 leader
// 必须等旧 leader 释放锁才能写，因此它的写总在后面。
//
// 时间线（forgetTimeout = 100ms，store 写入阻塞）：
//
//	t=0    请求 A 成为 leader，handler 返回 "v1"，取锁、检查通过（尚未淘汰），
//	       随后卡在 store 写入里
//	t=100  定时器触发：置位 A 的淘汰标记并 Forget
//	t=150  请求 B 进入成为新 leader，handler 返回 "v2"，取锁时被 A 挡住
//	t=200  放行 A 的 store 写入；A 写完 "v1" 并释放锁，B 随后写入 "v2"
//	t=250  请求 C 读缓存：必须拿到 "v2"
func TestSupersessionBetweenCheckAndWriteIsCaught(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const forgetTimeout = 100 * time.Millisecond

	inner := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = inner.Close() })

	store := &blockingWriteStore{
		CacheStore: inner,
		release:    make(chan struct{}),
		blocked:    make(chan struct{}),
	}

	var calls atomic.Int32
	engine := gin.New()
	engine.Use(Cache(store, time.Minute,
		WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
			return true, Strategy{CacheKey: c.Request.URL.Path}
		}),
		WithSingleFlightForgetTimeout(forgetTimeout),
	))
	engine.GET("/x", func(c *gin.Context) {
		c.String(http.StatusOK, "v%d", calls.Add(1))
	})

	serve := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		return rec
	}

	var wg sync.WaitGroup
	wg.Go(func() { serve() }) // 请求 A：卡在 store 写入里

	store.waitBlocked(t)               // A 已取锁并通过检查
	time.Sleep(150 * time.Millisecond) // 越过淘汰点

	wg.Go(func() { serve() }) // 请求 B：新 leader，取锁时被 A 挡住
	time.Sleep(50 * time.Millisecond)

	close(store.release) // 放行 A 的写入
	wg.Wait()

	if got := serve().Body.String(); got != "v2" {
		t.Fatalf("请求 C body = %q, want \"v2\"——旧 leader 在检查之后被淘汰却仍覆盖了新响应", got)
	}
}

// blockingWriteStore 让第一次缓存写入阻塞，用于把"检查通过 → 淘汰 → 新 leader 写入
// → 旧 leader 写入"这条交错钉成确定顺序。
type blockingWriteStore struct {
	persist.CacheStore

	release chan struct{}
	blocked chan struct{}
	first   atomic.Bool
}

func (s *blockingWriteStore) waitBlocked(t *testing.T) {
	t.Helper()
	select {
	case <-s.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("等待首次缓存写入进入阻塞超时")
	}
}

func (s *blockingWriteStore) SetWithContext(ctx context.Context, key string, value any, expire time.Duration) error {
	if s.first.CompareAndSwap(false, true) {
		close(s.blocked)
		<-s.release
	}
	return s.CacheStore.SetWithContext(ctx, key, value, expire)
}

// TestUnsupersededLeaderStillWritesCache 钉住未超时的 leader 照常写缓存。
//
// fencing 只该拦被淘汰的那一个，不能顺手把正常路径也拦掉。
func TestUnsupersededLeaderStillWritesCache(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls atomic.Int32

	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() { _ = store.Close() })

	engine := gin.New()
	engine.Use(Cache(store, time.Minute,
		WithCacheStrategyByRequest(func(c *gin.Context) (bool, Strategy) {
			return true, Strategy{CacheKey: c.Request.URL.Path}
		}),
		// 释放超时远大于 handler 耗时，这条 flight 不会被淘汰。
		WithSingleFlightForgetTimeout(10*time.Second),
	))
	engine.GET("/x", func(c *gin.Context) {
		c.String(http.StatusOK, "r%d", calls.Add(1))
	})

	serve := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		return rec
	}

	if got := serve().Body.String(); got != "r1" {
		t.Fatalf("首次请求 body = %q, want \"r1\"", got)
	}

	second := serve()
	if got := second.Body.String(); got != "r1" {
		t.Fatalf("第二次请求 body = %q, want \"r1\"（应命中缓存）", got)
	}
	if got := second.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("第二次请求 X-Cache = %q, want HIT——leader 没有写缓存", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler 调用次数 = %d, want 1", got)
	}
}
