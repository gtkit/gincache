package persist

import (
	"context"
	"errors"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestRedisStoreOperations(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
	})

	store := NewRedisStore(client,
		WithKeyPrefix("test:"),
		WithReadTimeout(time.Second),
		WithWriteTimeout(time.Second),
	)

	if err := store.Ping(context.Background()); err != nil {
		t.Fatalf("Ping error: %v", err)
	}

	if err := store.Set("user:1", map[string]any{"name": "alice"}, time.Minute); err != nil {
		t.Fatalf("Set error: %v", err)
	}
	if err := store.SetWithContext(context.Background(), "user:2", map[string]any{"name": "bob"}, time.Minute); err != nil {
		t.Fatalf("SetWithContext error: %v", err)
	}

	var got map[string]any
	if err := store.Get("user:1", &got); err != nil {
		t.Fatalf("Get user:1 error: %v", err)
	}
	if got["name"] != "alice" {
		t.Fatalf("Get user:1 name = %v, want alice", got["name"])
	}

	exists, err := store.Exists(context.Background(), "user:1")
	if err != nil {
		t.Fatalf("Exists error: %v", err)
	}
	if !exists {
		t.Fatal("Exists user:1 = false, want true")
	}

	ttl, err := store.TTL(context.Background(), "user:1")
	if err != nil {
		t.Fatalf("TTL error: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("TTL = %v, want positive", ttl)
	}

	stats := store.Stats()
	if stats["keys"] != 2 {
		t.Fatalf("Stats keys = %d, want 2", stats["keys"])
	}

	if err := store.DeleteWithContext(context.Background(), "user:1"); err != nil {
		t.Fatalf("DeleteWithContext error: %v", err)
	}
	if err := store.Get("user:1", &got); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("Get deleted key error = %v, want %v", err, ErrCacheMiss)
	}

	if err := store.DeleteKeys(context.Background(), "user:2"); err != nil {
		t.Fatalf("DeleteKeys error: %v", err)
	}
	if err := store.DeleteKeys(context.Background()); err != nil {
		t.Fatalf("DeleteKeys empty error: %v", err)
	}

	if store.Client() != client {
		t.Fatal("Client returned unexpected redis client")
	}
}

func TestRedisStoreDeletePattern(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
	})

	store := NewRedisStore(client, WithKeyPrefix("test:"))
	for _, key := range []string{"user:1", "user:2", "order:1"} {
		if err := store.Set(key, map[string]any{"key": key}, time.Minute); err != nil {
			t.Fatalf("Set(%s) error: %v", key, err)
		}
	}

	deleted, err := store.DeletePattern(context.Background(), "user:*")
	if err != nil {
		t.Fatalf("DeletePattern error: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("DeletePattern deleted = %d, want 2", deleted)
	}

	var got map[string]any
	if err := store.Get("user:1", &got); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("Get user:1 error = %v, want %v", err, ErrCacheMiss)
	}
	if err := store.Get("order:1", &got); err != nil {
		t.Fatalf("Get order:1 error: %v", err)
	}
}
