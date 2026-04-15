package gincache

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gtkit/gincache/persist"
)

func TestMiddlewareSingleflightSharesResponse(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() {
		_ = store.Close()
	})

	var calls atomic.Int32

	router := gin.New()
	router.GET("/items/:id",
		CacheByRequestPath(store, time.Minute),
		func(c *gin.Context) {
			n := calls.Add(1)
			time.Sleep(50 * time.Millisecond)
			c.JSON(http.StatusOK, gin.H{
				"id":    c.Param("id"),
				"call":  n,
				"value": "ok",
			})
		},
	)

	const requestCount = 8
	var wg sync.WaitGroup
	responses := make([]*httptest.ResponseRecorder, requestCount)

	for i := range requestCount {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodGet, "/items/42", nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			responses[i] = rec
		})
	}

	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("handler called %d times, want 1", got)
	}

	var body string
	for i, rec := range responses {
		if rec.Code != http.StatusOK {
			t.Fatalf("response %d status = %d, want %d", i, rec.Code, http.StatusOK)
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("response %d body is empty", i)
		}
		if i == 0 {
			body = rec.Body.String()
			continue
		}
		if rec.Body.String() != body {
			t.Fatalf("response %d body = %s, want %s", i, rec.Body.String(), body)
		}
	}
}

func TestMiddlewareCacheReplay(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() {
		_ = store.Close()
	})

	router := gin.New()
	router.GET("/cached",
		CacheByRequestPath(store, time.Minute),
		func(c *gin.Context) {
			c.Header("ETag", "etag-1")
			c.Header("Cache-Control", "public, max-age=60")
			c.Data(http.StatusAccepted, "application/json; charset=utf-8", []byte(`{"ok":true}`))
		},
	)

	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/cached", nil))

	second := httptest.NewRecorder()
	router.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/cached", nil))

	if second.Code != http.StatusAccepted {
		t.Fatalf("cached status = %d, want %d", second.Code, http.StatusAccepted)
	}
	if got := second.Header().Get("ETag"); got != "etag-1" {
		t.Fatalf("cached ETag = %q, want %q", got, "etag-1")
	}
	if got := second.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("cached Cache-Control = %q, want %q", got, "public, max-age=60")
	}
	if got := second.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("cached X-Cache = %q, want %q", got, "HIT")
	}
	if got := second.Body.String(); got != `{"ok":true}` {
		t.Fatalf("cached body = %q, want %q", got, `{"ok":true}`)
	}
}

func TestMiddlewareCacheReplayPreservesFullHeaders(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() {
		_ = store.Close()
	})

	router := gin.New()
	router.GET("/headers",
		CacheByRequestPath(store, time.Minute),
		func(c *gin.Context) {
			c.Header("Vary", "Accept-Encoding")
			c.Header("X-Trace", "trace-1")
			c.Writer.Header().Add("Link", `</a.css>; rel=preload; as=style`)
			c.Writer.Header().Add("Link", `</b.js>; rel=preload; as=script`)
			c.Data(http.StatusOK, "application/json; charset=utf-8", []byte(`{"ok":true}`))
		},
	)

	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/headers", nil))

	second := httptest.NewRecorder()
	router.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/headers", nil))

	if got := second.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("cached Vary = %q, want %q", got, "Accept-Encoding")
	}
	if got := second.Header().Get("X-Trace"); got != "trace-1" {
		t.Fatalf("cached X-Trace = %q, want %q", got, "trace-1")
	}
	if got := second.Header().Values("Link"); len(got) != 2 || got[0] != `</a.css>; rel=preload; as=style` || got[1] != `</b.js>; rel=preload; as=script` {
		t.Fatalf("cached Link = %#v, want both values preserved", got)
	}
}

func TestMiddlewareCacheableStatusDefaults(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() {
		_ = store.Close()
	})

	var calls atomic.Int32

	router := gin.New()
	router.GET("/status",
		CacheByRequestPath(store, time.Minute),
		func(c *gin.Context) {
			n := calls.Add(1)
			c.JSON(http.StatusCreated, gin.H{"call": n})
		},
	)

	for range 2 {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("handler called %d times, want 1 for default 2xx caching", got)
	}
}

func TestMiddlewareCacheableStatusOverride(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	store := persist.NewMemoryStore(time.Minute)
	t.Cleanup(func() {
		_ = store.Close()
	})

	var calls atomic.Int32

	router := gin.New()
	router.GET("/status",
		CacheByRequestPath(store, time.Minute, WithCacheableStatusCodes(http.StatusNotModified)),
		func(c *gin.Context) {
			n := calls.Add(1)
			c.Status(http.StatusNotModified)
			c.Header("ETag", "etag-2")
			_, _ = c.Writer.Write([]byte(fmt.Sprintf(`{"call":%d}`, n)))
		},
	)

	for range 2 {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("handler called %d times, want 1 for configured status caching", got)
	}
}

func TestCachedWriterStopsBufferingAfterMaxBodySize(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	cw := getWriter(ctx.Writer, 4)
	t.Cleanup(func() {
		putWriter(cw)
	})

	if _, err := cw.Write([]byte("ab")); err != nil {
		t.Fatalf("first write error: %v", err)
	}
	if cw.overflowed {
		t.Fatal("writer overflowed too early")
	}
	if got := cw.body.String(); got != "ab" {
		t.Fatalf("buffer after first write = %q, want %q", got, "ab")
	}

	if _, err := cw.Write([]byte("cde")); err != nil {
		t.Fatalf("second write error: %v", err)
	}
	if !cw.overflowed {
		t.Fatal("writer should mark overflow after exceeding max body size")
	}
	if got := cw.body.Len(); got != 0 {
		t.Fatalf("buffer length after overflow = %d, want 0", got)
	}
}
