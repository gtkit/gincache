// Package adaptertest 提供仅供适配器测试使用的公共契约。
package adaptertest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gtkit/gincache/persist"
	"github.com/gtkit/gincache/persist/internal/localadapter"
)

type Owner = localadapter.Owner
type Store = localadapter.Store
type Option = localadapter.Option

var (
	WithClock             = localadapter.WithClock
	WithDefaultExpiration = localadapter.WithDefaultExpiration
	ErrInvalidArgument    = localadapter.ErrInvalidArgument
	ErrClosed             = localadapter.ErrClosed
)

func CloseOwner(t *testing.T, owner *Owner) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Second)
		defer cancel()
		if err := owner.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
}

func NewStore(t *testing.T, owner *Owner, namespace string, opts ...Option) *Store {
	t.Helper()
	store, err := owner.NewStore(namespace, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func Run(t *testing.T, factory func(*testing.T) *Owner) {
	t.Helper()
	t.Run("namespaces and borrowed views", func(t *testing.T) {
		owner := factory(t)
		a, b := NewStore(t, owner, "api:a"), NewStore(t, owner, "api:a:b")
		for _, item := range []struct {
			store *Store
			value string
		}{{a, "business"}, {b, "http"}} {
			if err := item.store.Set("同一个/key", item.value, time.Minute); err != nil {
				t.Fatal(err)
			}
		}
		if err := a.Delete("同一个/key"); err != nil {
			t.Fatal(err)
		}
		if _, err := a.GetBytes("同一个/key"); !errors.Is(err, persist.ErrCacheMiss) {
			t.Fatalf("miss: %v", err)
		}
		var got string
		if err := b.Get("同一个/key", &got); err != nil || got != "http" {
			t.Fatalf("other namespace: %q %v", got, err)
		}
		a.ResetStats()
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		if err := b.Get("同一个/key", &got); err != nil {
			t.Fatal(err)
		}
		if err := a.Set("x", "v", 0); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed set: %v", err)
		}
		if _, err := owner.NewStore("api:a"); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("duplicate: %v", err)
		}
		if b.Stats()["keys"] != 1 {
			t.Fatalf("stats: %v", b.Stats())
		}
	})
	t.Run("precise expiry and default", func(t *testing.T) {
		owner := factory(t)
		var offset atomic.Int64
		start := time.Now()
		now := func() time.Time { return start.Add(time.Duration(offset.Load())) }
		store := NewStore(t, owner, "ttl", WithClock(now), WithDefaultExpiration(5*time.Millisecond))
		if err := store.Set("key", "old", 0); err != nil {
			t.Fatal(err)
		}
		offset.Store(int64(5 * time.Millisecond))
		if _, err := store.GetBytes("key"); !errors.Is(err, persist.ErrCacheMiss) {
			t.Fatalf("expired at equality: %v", err)
		}
		if err := store.Set("key", "new", time.Minute); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := store.Get("key", &got); err != nil || got != "new" {
			t.Fatalf("replacement: %q %v", got, err)
		}
		if _, err := store.SetBytes("negative", nil, -1); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("negative: %v", err)
		}
	})
	t.Run("bytes and corruption", func(t *testing.T) {
		owner := factory(t)
		store := NewStore(t, owner, "bytes")
		value := []byte("payload")
		ok, err := store.SetBytes("", value, 0)
		if err != nil || !ok {
			t.Fatalf("SetBytes: %t %v", ok, err)
		}
		value[0] = 'X'
		got, err := store.GetBytes("")
		if err != nil || string(got) != "payload" {
			t.Fatalf("alias: %q %v", got, err)
		}
		got[0] = 'Y'
		again, err := store.GetBytes("")
		if err != nil || string(again) != "payload" {
			t.Fatalf("returned alias: %q %v", again, err)
		}
	})
	t.Run("context and close", func(t *testing.T) {
		owner := factory(t)
		store := NewStore(t, owner, "close")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := store.SetWithContext(ctx, "key", "value", 0); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
		if err := owner.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := owner.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := store.GetBytes("key"); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed get: %v", err)
		}
		if err := store.Delete("key"); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed delete: %v", err)
		}
		if store.Stats()["closed"] != 1 {
			t.Fatal("closed stats")
		}
	})
}
