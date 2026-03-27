package persist

import (
	"context"
	"errors"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestTwoLevelStoreSetDoesNotKeepLocalOnRemoteFailure(t *testing.T) {
	t.Parallel()

	client := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  50 * time.Millisecond,
		ReadTimeout:  50 * time.Millisecond,
		WriteTimeout: 50 * time.Millisecond,
	})
	t.Cleanup(func() {
		_ = client.Close()
	})

	store := NewTwoLevelStore(client, WithLocalTTL(time.Minute), WithRemoteTTL(time.Minute))
	t.Cleanup(func() {
		_ = store.Close()
	})

	value := map[string]any{"name": "broken"}
	err := store.SetWithContext(context.Background(), "user:1", value, time.Minute)
	if err == nil {
		t.Fatal("expected remote write failure")
	}

	var got map[string]any
	if err := store.local.Get("user:1", &got); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("local cache err = %v, want %v", err, ErrCacheMiss)
	}
}

func TestTwoLevelStoreBackfillsLocalAfterRemoteHit(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
	})

	store := NewTwoLevelStore(client, WithLocalTTL(time.Minute), WithRemoteTTL(time.Minute))
	t.Cleanup(func() {
		_ = store.Close()
	})

	value := map[string]any{"name": "alice"}
	if err := store.remote.Set("user:2", value, time.Minute); err != nil {
		t.Fatalf("remote set error: %v", err)
	}

	var got map[string]any
	if err := store.Get("user:2", &got); err != nil {
		t.Fatalf("store.Get error: %v", err)
	}
	if got["name"] != "alice" {
		t.Fatalf("store.Get name = %v, want alice", got["name"])
	}

	var local map[string]any
	if err := store.local.Get("user:2", &local); err != nil {
		t.Fatalf("local.Get error: %v", err)
	}
	if local["name"] != "alice" {
		t.Fatalf("local name = %v, want alice", local["name"])
	}
}
