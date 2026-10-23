package localadapter_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gtkit/gincache/persist"
	"github.com/gtkit/gincache/persist/internal/adaptertest"
	"github.com/gtkit/gincache/persist/internal/localadapter"
)

type memoryEngine struct {
	values map[string][]byte
	closes int
}

func (e *memoryEngine) Get(key string) ([]byte, error) {
	value, ok := e.values[key]
	if !ok {
		return nil, persist.ErrCacheMiss
	}
	return slices.Clone(value), nil
}

func (e *memoryEngine) Set(key string, value []byte, _ time.Duration) (bool, error) {
	e.values[key] = slices.Clone(value)
	return true, nil
}
func (e *memoryEngine) Delete(key string) error { delete(e.values, key); return nil }
func (e *memoryEngine) Walk(visit func(string, []byte)) error {
	for key, value := range e.values {
		visit(key, value)
	}
	return nil
}
func (e *memoryEngine) Close() error { e.closes++; return nil }

func newMemoryOwner() (*localadapter.Owner, *memoryEngine) {
	e := &memoryEngine{values: make(map[string][]byte)}
	return localadapter.NewOwner(e, true), e
}

func TestStoreContract(t *testing.T) {
	adaptertest.Run(t, func(t *testing.T) *localadapter.Owner {
		owner, _ := newMemoryOwner()
		adaptertest.CloseOwner(t, owner)
		return owner
	})
}

func TestValidationCorruptionAndOwnership(t *testing.T) {
	owner, engine := newMemoryOwner()
	adaptertest.CloseOwner(t, owner)
	for _, ns := range []string{"", "bad\x00key", "\xff"} {
		if _, err := owner.NewStore(ns); !errors.Is(err, localadapter.ErrInvalidArgument) {
			t.Fatalf("namespace: %v", err)
		}
	}
	if _, err := owner.NewStore("nil-clock", localadapter.WithClock(nil)); !errors.Is(err, localadapter.ErrInvalidArgument) {
		t.Fatal(err)
	}
	if _, err := owner.NewStore("negative", localadapter.WithDefaultExpiration(-1)); !errors.Is(err, localadapter.ErrInvalidArgument) {
		t.Fatal(err)
	}
	store := adaptertest.NewStore(t, owner, "corrupt")
	engine.values["gac2:7:corrupt:key"] = []byte("invalid")
	if _, err := store.GetBytes("key"); !errors.Is(err, localadapter.ErrCorruptEntry) {
		t.Fatal(err)
	}
	if err := store.Delete("key"); !errors.Is(err, localadapter.ErrCorruptEntry) {
		t.Fatal(err)
	}
	if err := owner.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(t.Context()); err != nil || engine.closes != 1 {
		t.Fatalf("close: %v, count %d", err, engine.closes)
	}
}

type advancingJSON struct{ advance func() }

func (v advancingJSON) MarshalJSON() ([]byte, error) {
	v.advance()
	return []byte(`"encoded"`), nil
}

func TestJSONEncodingConsumesTTL(t *testing.T) {
	owner, _ := newMemoryOwner()
	adaptertest.CloseOwner(t, owner)
	now := time.Now()
	store := adaptertest.NewStore(t, owner, "encoding", localadapter.WithClock(func() time.Time { return now }))
	if err := store.Set("key", advancingJSON{func() { now = now.Add(time.Second) }}, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetBytes("key"); !errors.Is(err, persist.ErrCacheMiss) {
		t.Fatalf("encoding extended TTL: %v", err)
	}
}

type blockingEngine struct {
	memoryEngine
	entered chan struct{}
	release chan struct{}
}

func (e *blockingEngine) Set(key string, value []byte, ttl time.Duration) (bool, error) {
	close(e.entered)
	<-e.release
	return e.memoryEngine.Set(key, value, ttl)
}

func TestCloseCancellationDoesNotReleaseInFlightEngine(t *testing.T) {
	e := &blockingEngine{values: make(map[string][]byte), entered: make(chan struct{}), release: make(chan struct{})}
	owner := localadapter.NewOwner(e, true)
	store := adaptertest.NewStore(t, owner, "blocked")
	done := make(chan error, 1)
	var group sync.WaitGroup
	group.Go(func() { done <- store.Set("key", "value", 0) })
	<-e.entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := owner.Close(ctx); !errors.Is(err, context.Canceled) || e.closes != 0 {
		t.Errorf("close in flight: %v, %d", err, e.closes)
	}
	close(e.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	group.Wait()
	if err := owner.Close(t.Context()); err != nil || e.closes != 1 {
		t.Fatalf("final close: %v, %d", err, e.closes)
	}
}
