package adaptertest

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/gtkit/gincache"
	"github.com/gtkit/gincache/persist"
	"github.com/redis/go-redis/v9"
)

// RunIntegration 验证真实 gincache 中间件与 TwoLevelStore 接入；Redis 使用内存 fixture。
func RunIntegration(t *testing.T, owner *Owner) {
	t.Helper()
	peer := NewStore(t, owner, "business")
	if err := peer.Set("key", "business", time.Minute); err != nil {
		t.Fatal(err)
	}
	httpStore := NewStore(t, owner, "http")
	router := gin.New()
	calls := 0
	router.GET("/public", gincache.Cache(httpStore, time.Minute,
		gincache.WithCacheStrategyByRequest(func(c *gin.Context) (bool, gincache.Strategy) {
			return true, gincache.Strategy{CacheKey: c.Request.URL.Path}
		})), func(c *gin.Context) {
		calls++
		c.String(http.StatusOK, "response")
	})
	for range 2 {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/public", nil))
		if response.Code != http.StatusOK || response.Body.String() != "response" {
			t.Fatalf("HTTP: %d %q", response.Code, response.Body.String())
		}
	}
	if calls != 1 {
		t.Fatalf("HTTP misses: %d", calls)
	}
	if err := httpStore.Close(); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := peer.Get("key", &value); err != nil || value != "business" {
		t.Fatalf("shared owner: %q %v", value, err)
	}

	now := time.Now()
	l1 := NewStore(t, owner, "l1", WithClock(func() time.Time { return now }), WithDefaultExpiration(time.Hour))
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	level := persist.NewTwoLevelStore(client, persist.WithLocalStore(l1), persist.WithLocalTTL(time.Minute))
	if err := level.SetWithContext(t.Context(), "key", "remote", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := level.Get("key", &value); err != nil || value != "remote" {
		t.Fatalf("L1 fill: %q %v", value, err)
	}
	server.FastForward(time.Second)
	now = now.Add(time.Second)
	if err := level.Get("key", &value); !errors.Is(err, persist.ErrCacheMiss) {
		t.Fatalf("L1 expiry: %v", err)
	}
	if err := level.Close(); err != nil {
		t.Fatal(err)
	}
	if err := peer.Get("key", &value); err != nil || value != "business" {
		t.Fatalf("L1 close affected peer: %q %v", value, err)
	}
}
