package ristrettoadapter

import (
	"context"
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
	if err := store.Get("user:1", &got); err != persist.ErrCacheMiss {
		t.Fatalf("Get user:1 err = %v, want %v", err, persist.ErrCacheMiss)
	}
	if err := store.Get("user:2", &got); err != persist.ErrCacheMiss {
		t.Fatalf("Get user:2 err = %v, want %v", err, persist.ErrCacheMiss)
	}
	if err := store.Get("order:1", &got); err != nil {
		t.Fatalf("Get order:1 error: %v", err)
	}
}
