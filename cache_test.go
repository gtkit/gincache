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
