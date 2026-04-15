package ristrettoadapter

import (
	"context"
	"errors"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	ristretto "github.com/dgraph-io/ristretto/v2"
	"github.com/gtkit/gincache/persist"
	"github.com/redis/go-redis/v9"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	cache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: 1_000,
		MaxCost:     1 << 20,
		BufferItems: 64,
	})
	if err != nil {
		t.Fatalf("NewCache error: %v", err)
	}

	store := New(cache, WithWait())

	payload := map[string]any{"name": "alice"}
	if err := store.Set("user:1", payload, time.Minute); err != nil {
		t.Fatalf("Set error: %v", err)
	}

	var got map[string]any
	if err := store.Get("user:1", &got); err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if got["name"] != "alice" {
		t.Fatalf("Get name = %v, want alice", got["name"])
	}
}

func TestWithLocalStoreOptionUsesInjectedCache(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
	})

	cache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: 1_000,
		MaxCost:     1 << 20,
		BufferItems: 64,
	})
	if err != nil {
		t.Fatalf("NewCache error: %v", err)
	}

	store := persist.NewTwoLevelStore(client,
		persist.WithLocalTTL(time.Minute),
		persist.WithRemoteTTL(time.Minute),
		WithLocalStore(cache, WithWait()),
	)
	t.Cleanup(func() {
		_ = store.Close()
	})

	local, ok := store.LocalStore().(*Store)
	if !ok {
		t.Fatalf("local store type = %T, want *Store", store.LocalStore())
	}
	if local.Cache() != cache {
		t.Fatal("wrapped cache instance mismatch")
	}

	value := map[string]any{"name": "bob"}
	if err := store.SetWithContext(context.Background(), "user:2", value, time.Minute); err != nil {
		t.Fatalf("SetWithContext error: %v", err)
	}

	var got map[string]any
	if err := local.Get("user:2", &got); err != nil {
		t.Fatalf("local.Get error: %v", err)
	}
	if got["name"] != "bob" {
		t.Fatalf("local name = %v, want bob", got["name"])
	}
}

func TestDeletePattern(t *testing.T) {
	t.Parallel()

	cache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: 1_000,
		MaxCost:     1 << 20,
		BufferItems: 64,
	})
	if err != nil {
		t.Fatalf("NewCache error: %v", err)
	}

	store := New(cache, WithWait())

	for key, value := range map[string]map[string]any{
		"user:1":  {"v": 1},
		"user:2":  {"v": 2},
		"order:1": {"v": 3},
	} {
		if err := store.Set(key, value, time.Minute); err != nil {
			t.Fatalf("Set(%s) error: %v", key, err)
		}
	}

	if _, err := store.DeletePattern(context.Background(), "user:*"); err != nil {
		t.Fatalf("DeletePattern error: %v", err)
	}

	var got map[string]any
	if err := store.Get("user:1", &got); !errors.Is(err, persist.ErrCacheMiss) {
		t.Fatalf("Get user:1 err = %v, want %v", err, persist.ErrCacheMiss)
	}
	if err := store.Get("user:2", &got); !errors.Is(err, persist.ErrCacheMiss) {
		t.Fatalf("Get user:2 err = %v, want %v", err, persist.ErrCacheMiss)
	}
	if err := store.Get("order:1", &got); err != nil {
		t.Fatalf("Get order:1 error: %v", err)
	}
}

func TestDeletePatternMatchesWildcardInMiddle(t *testing.T) {
	t.Parallel()

	cache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: 1_000,
		MaxCost:     1 << 20,
		BufferItems: 64,
	})
	if err != nil {
		t.Fatalf("NewCache error: %v", err)
	}

	store := New(cache, WithWait())

	for key, value := range map[string]map[string]any{
		"user:1:profile":  {"v": 1},
		"user:2:profile":  {"v": 2},
		"user:1:settings": {"v": 3},
	} {
		if err := store.Set(key, value, time.Minute); err != nil {
			t.Fatalf("Set(%s) error: %v", key, err)
		}
	}

	if _, err := store.DeletePattern(context.Background(), "user:*:profile"); err != nil {
		t.Fatalf("DeletePattern error: %v", err)
	}

	var got map[string]any
	if err := store.Get("user:1:profile", &got); !errors.Is(err, persist.ErrCacheMiss) {
		t.Fatalf("Get user:1:profile err = %v, want %v", err, persist.ErrCacheMiss)
	}
	if err := store.Get("user:2:profile", &got); !errors.Is(err, persist.ErrCacheMiss) {
		t.Fatalf("Get user:2:profile err = %v, want %v", err, persist.ErrCacheMiss)
	}
	if err := store.Get("user:1:settings", &got); err != nil {
		t.Fatalf("Get user:1:settings error: %v", err)
	}
}

func TestGetMissDoesNotUntrackKey(t *testing.T) {
	t.Parallel()

	cache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: 1_000,
		MaxCost:     1 << 20,
		BufferItems: 64,
	})
	if err != nil {
		t.Fatalf("NewCache error: %v", err)
	}

	store := New(cache)
	store.trackKey("user:pending")

	if got := store.keyCount.Load(); got != 1 {
		t.Fatalf("tracked before miss = %d, want 1", got)
	}

	var got map[string]any
	if err := store.Get("user:pending", &got); !errors.Is(err, persist.ErrCacheMiss) {
		t.Fatalf("Get err = %v, want %v", err, persist.ErrCacheMiss)
	}

	if got := store.keyCount.Load(); got != 1 {
		t.Fatalf("tracked after miss = %d, want 1", got)
	}
}

func TestStatsPrunesTrackedKeysEvictedFromCache(t *testing.T) {
	t.Parallel()

	cache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters:        1_000,
		MaxCost:            1,
		BufferItems:        64,
		IgnoreInternalCost: true,
	})
	if err != nil {
		t.Fatalf("NewCache error: %v", err)
	}

	store := New(cache, WithWait())

	keys := []string{"user:1", "user:2", "user:3"}
	for _, key := range keys {
		if err := store.Set(key, map[string]any{"key": key}, time.Minute); err != nil {
			t.Fatalf("Set(%s) error: %v", key, err)
		}
	}

	var live int64
	for _, key := range keys {
		if _, ok := cache.Get(key); ok {
			live++
		}
	}

	stats := store.Stats()
	if stats["tracked"] != live {
		t.Fatalf("tracked after eviction = %d, want %d live keys", stats["tracked"], live)
	}
}
