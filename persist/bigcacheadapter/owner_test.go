package bigcacheadapter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/allegro/bigcache/v3"
	"github.com/gtkit/gincache/persist"
	"github.com/gtkit/gincache/persist/internal/adaptertest"
)

func bigConfig() bigcache.Config {
	return bigcache.Config{
		Shards: 16, HardMaxCacheSize: 16, LifeWindow: time.Hour,
		MaxEntriesInWindow: 256, MaxEntrySize: 512,
	}
}

func TestBigCacheContract(t *testing.T) {
	adaptertest.Run(t, func(t *testing.T) *Owner {
		owner, err := NewOwner(t.Context(), bigConfig())
		if err != nil {
			t.Fatal(err)
		}
		adaptertest.CloseOwner(t, owner)
		return owner
	})
}

type collidingHasher struct{}

func (collidingHasher) Sum64(string) uint64 { return 1 }

func TestBigCacheCollisionCannotReadOrDeleteOtherNamespace(t *testing.T) {
	cfg := bigConfig()
	cfg.Hasher = collidingHasher{}
	owner, err := NewOwner(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.CloseOwner(t, owner)
	a, b := adaptertest.NewStore(t, owner, "a"), adaptertest.NewStore(t, owner, "b")
	if err := a.Set("key", "a", 0); err != nil {
		t.Fatal(err)
	}
	if err := b.Set("key", "b", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetBytes("key"); !errors.Is(err, persist.ErrCacheMiss) {
		t.Fatalf("collision read: %v", err)
	}
	if err := a.Delete("key"); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := b.Get("key", &got); err != nil || got != "b" {
		t.Fatalf("collision deletion: %q %v", got, err)
	}
}

func TestBigCacheBorrowAndValidation(t *testing.T) {
	cfg := bigConfig()
	raw, err := bigcache.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Error(err)
		}
	})
	owner, err := WrapOwner(raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := raw.Set("external", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Get("external"); err != nil {
		t.Fatal(err)
	}
	cfg.HardMaxCacheSize = 0
	if _, err := NewOwner(t.Context(), cfg); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("unbounded: %v", err)
	}
}

func TestBigCacheLargeEntryReturnsError(t *testing.T) {
	owner, err := NewOwner(t.Context(), bigConfig())
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.CloseOwner(t, owner)
	store := adaptertest.NewStore(t, owner, "large")
	if stored, err := store.SetBytes("key", make([]byte, 2<<20), 0); err == nil || stored {
		t.Fatalf("large entry must preserve native error: %t %v", stored, err)
	}
}

func TestGincacheAndL1Integration(t *testing.T) {
	owner, err := NewOwner(t.Context(), bigConfig())
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.CloseOwner(t, owner)
	adaptertest.RunIntegration(t, owner)
}

func TestBigCacheConstructorBoundaries(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, test := range []struct {
		name string
		ctx  context.Context
		want error
	}{{"nil context", nil, ErrInvalidArgument}, {"canceled context", ctx, context.Canceled}} {
		t.Run(test.name, func(t *testing.T) {
			if owner, err := NewOwner(test.ctx, bigConfig()); owner != nil || !errors.Is(err, test.want) {
				t.Fatalf("NewOwner: owner=%v, error=%v, want %v", owner, err, test.want)
			}
		})
	}
	if owner, err := WrapOwner(nil, bigConfig()); owner != nil || !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil native cache: owner=%v, error=%v", owner, err)
	}

	raw, err := bigcache.New(t.Context(), bigConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := raw.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, test := range []struct {
		name   string
		change func(*bigcache.Config)
	}{
		{"zero shards", func(cfg *bigcache.Config) { cfg.Shards = 0 }},
		{"non power of two shards", func(cfg *bigcache.Config) { cfg.Shards = 3 }},
		{"unbounded capacity", func(cfg *bigcache.Config) { cfg.HardMaxCacheSize = 0 }},
		{"overflowing capacity", func(cfg *bigcache.Config) { cfg.HardMaxCacheSize = int(^uint(0)>>1)/(1<<20) + 1 }},
		{"negative entry size", func(cfg *bigcache.Config) { cfg.MaxEntrySize = -1 }},
		{"negative entry count", func(cfg *bigcache.Config) { cfg.MaxEntriesInWindow = -1 }},
		{"negative lifetime", func(cfg *bigcache.Config) { cfg.LifeWindow = -1 }},
		{"subsecond cleanup lifetime", func(cfg *bigcache.Config) { cfg.CleanWindow = time.Second; cfg.LifeWindow = time.Millisecond }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := bigConfig()
			test.change(&cfg)
			if owner, err := NewOwner(t.Context(), cfg); owner != nil || !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("invalid construction: owner=%v, error=%v", owner, err)
			}
			if owner, err := WrapOwner(raw, cfg); owner != nil || !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("invalid wrapping: owner=%v, error=%v", owner, err)
			}
		})
	}
	if err := raw.Set("after-rejected-wrap", []byte("available")); err != nil {
		t.Fatal(err)
	}
	if got, err := raw.Get("after-rejected-wrap"); err != nil || string(got) != "available" {
		t.Fatalf("rejected wrapping changed native cache: value=%q, error=%v", got, err)
	}
}

func TestBigCachePublicOptionsPreserveDefaultTTL(t *testing.T) {
	owner, err := NewOwner(t.Context(), bigConfig())
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.CloseOwner(t, owner)
	now := time.Now()
	store, err := owner.NewStore("public-options", WithClock(func() time.Time { return now }), WithStoreExpiration(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("key", "value", 0); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := store.Get("key", &got); err != nil || got != "value" {
		t.Fatalf("before default expiry: value=%q, error=%v", got, err)
	}
	now = now.Add(10 * time.Millisecond)
	if _, err := store.GetBytes("key"); !errors.Is(err, persist.ErrCacheMiss) {
		t.Fatalf("at default expiry: %v", err)
	}
}
