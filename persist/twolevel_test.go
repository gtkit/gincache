package persist

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/gtkit/json"
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

func TestTwoLevelStoreInvalidationBroadcastClearsOtherLocalCachesOnDelete(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	clientA := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	clientB := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() {
		_ = clientA.Close()
		_ = clientB.Close()
	})

	storeA := NewTwoLevelStore(clientA,
		WithLocalTTL(time.Minute),
		WithRemoteTTL(time.Minute),
		WithTwoLevelInvalidationBroadcast(clientA, "gincache:test:invalidate"),
	)
	storeB := NewTwoLevelStore(clientB,
		WithLocalTTL(time.Minute),
		WithRemoteTTL(time.Minute),
		WithTwoLevelInvalidationBroadcast(clientB, "gincache:test:invalidate"),
	)
	t.Cleanup(func() {
		_ = storeA.Close()
		_ = storeB.Close()
	})
	waitForSubscribers(t, mini, "gincache:test:invalidate", 2)

	if err := storeA.Set("route:/products", map[string]any{"version": "v1"}, time.Minute); err != nil {
		t.Fatalf("storeA.Set error: %v", err)
	}

	var got map[string]any
	if err := storeB.Get("route:/products", &got); err != nil {
		t.Fatalf("storeB.Get error: %v", err)
	}

	if err := storeA.Delete("route:/products"); err != nil {
		t.Fatalf("storeA.Delete error: %v", err)
	}

	eventually(t, time.Second, func() bool {
		var cached map[string]any
		return errors.Is(storeB.local.Get("route:/products", &cached), ErrCacheMiss)
	})
}

func TestTwoLevelStoreInvalidationBroadcastClearsOtherLocalCachesOnSet(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	clientA := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	clientB := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() {
		_ = clientA.Close()
		_ = clientB.Close()
	})

	storeA := NewTwoLevelStore(clientA,
		WithLocalTTL(time.Minute),
		WithRemoteTTL(time.Minute),
		WithTwoLevelInvalidationBroadcast(clientA, "gincache:test:set-invalidate"),
	)
	storeB := NewTwoLevelStore(clientB,
		WithLocalTTL(time.Minute),
		WithRemoteTTL(time.Minute),
		WithTwoLevelInvalidationBroadcast(clientB, "gincache:test:set-invalidate"),
	)
	t.Cleanup(func() {
		_ = storeA.Close()
		_ = storeB.Close()
	})
	waitForSubscribers(t, mini, "gincache:test:set-invalidate", 2)

	if err := storeA.Set("route:/products", map[string]any{"version": "v1"}, time.Minute); err != nil {
		t.Fatalf("storeA.Set v1 error: %v", err)
	}

	var got map[string]any
	if err := storeB.Get("route:/products", &got); err != nil {
		t.Fatalf("storeB.Get error: %v", err)
	}
	if got["version"] != "v1" {
		t.Fatalf("storeB.Get version = %v, want v1", got["version"])
	}

	if err := storeA.Set("route:/products", map[string]any{"version": "v2"}, time.Minute); err != nil {
		t.Fatalf("storeA.Set v2 error: %v", err)
	}

	eventually(t, time.Second, func() bool {
		var cached map[string]any
		return errors.Is(storeB.local.Get("route:/products", &cached), ErrCacheMiss)
	})

	if err := storeB.Get("route:/products", &got); err != nil {
		t.Fatalf("storeB.Get after invalidation error: %v", err)
	}
	if got["version"] != "v2" {
		t.Fatalf("storeB.Get version after invalidation = %v, want v2", got["version"])
	}
}

func TestTwoLevelStoreInvalidationBroadcastPublishesAfterRemoteSetEvenIfLocalSetFails(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	clientA := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	clientB := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() {
		_ = clientA.Close()
		_ = clientB.Close()
	})

	storeA := NewTwoLevelStore(clientA,
		WithLocalTTL(time.Minute),
		WithRemoteTTL(time.Minute),
		WithLocalStore(failingSetLocalStore{}),
		WithTwoLevelInvalidationBroadcast(clientA, "gincache:test:set-fail-invalidate"),
	)
	storeB := NewTwoLevelStore(clientB,
		WithLocalTTL(time.Minute),
		WithRemoteTTL(time.Minute),
		WithTwoLevelInvalidationBroadcast(clientB, "gincache:test:set-fail-invalidate"),
	)
	t.Cleanup(func() {
		_ = storeA.Close()
		_ = storeB.Close()
	})
	waitForSubscribers(t, mini, "gincache:test:set-fail-invalidate", 2)

	if err := storeB.Set("route:/products", map[string]any{"version": "v1"}, time.Minute); err != nil {
		t.Fatalf("storeB.Set v1 error: %v", err)
	}

	var got map[string]any
	if err := storeB.Get("route:/products", &got); err != nil {
		t.Fatalf("storeB.Get error: %v", err)
	}
	if got["version"] != "v1" {
		t.Fatalf("storeB.Get version = %v, want v1", got["version"])
	}

	if err := storeA.Set("route:/products", map[string]any{"version": "v2"}, time.Minute); !errors.Is(err, errLocalSetFailed) {
		t.Fatalf("storeA.Set error = %v, want %v", err, errLocalSetFailed)
	}

	eventually(t, time.Second, func() bool {
		var cached map[string]any
		return errors.Is(storeB.local.Get("route:/products", &cached), ErrCacheMiss)
	})
}

func TestTwoLevelStoreSingleFlightForgetTimeoutAllowsNewLeader(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
	})

	store := NewTwoLevelStore(client,
		WithLocalTTL(time.Minute),
		WithRemoteTTL(time.Minute),
		WithLocalStore(newBlockingLocalStore()),
		WithTwoLevelSingleFlightForgetTimeout(20*time.Millisecond),
	)
	t.Cleanup(func() {
		_ = store.Close()
	})

	const key = "route:/slow"
	firstGetDone := make(chan struct{})
	go func() {
		var got map[string]any
		_ = store.Get(key, &got)
		close(firstGetDone)
	}()

	time.Sleep(50 * time.Millisecond)

	if err := store.remote.Set(key, map[string]any{"version": "ready"}, time.Minute); err != nil {
		t.Fatalf("remote set error: %v", err)
	}

	var got map[string]any
	if err := store.Get(key, &got); err != nil {
		t.Fatalf("second Get error: %v", err)
	}
	if got["version"] != "ready" {
		t.Fatalf("second Get version = %v, want ready", got["version"])
	}

	select {
	case <-firstGetDone:
		t.Fatal("first Get unexpectedly completed")
	default:
	}
}

func TestTwoLevelStoreWithInvalidationBroadcastClosesImmediately(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
	})

	store := NewTwoLevelStore(client,
		WithTwoLevelInvalidationBroadcast(client, "gincache:test:close"),
	)

	done := make(chan error, 1)
	go func() {
		done <- store.Close()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("store.Close error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("store.Close timed out")
	}
}

func TestTwoLevelStoreInvalidationBroadcastDoesNotLogExpectedClose(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
	})

	logger := &captureLogger{}
	store := NewTwoLevelStore(client,
		WithTwoLevelLogger(logger),
		WithTwoLevelInvalidationBroadcast(client, "gincache:test:expected-close"),
	)
	waitForSubscribers(t, mini, "gincache:test:expected-close", 1)

	if err := store.Close(); err != nil {
		t.Fatalf("store.Close error: %v", err)
	}
	if logger.contains("invalidation subscriber channel closed") {
		t.Fatal("normal store.Close should not log subscriber channel closed")
	}
}

func TestTwoLevelStoreWithoutInvalidationBroadcastAcceptsCmdableOnlyClient(t *testing.T) {
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

	if err := store.Set("route:/products", map[string]any{"version": "v1"}, time.Minute); err != nil {
		t.Fatalf("store.Set error: %v", err)
	}
}

func TestTwoLevelStoreInvalidationBroadcastLogsInvalidPayload(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
	})

	logger := &captureLogger{}
	store := NewTwoLevelStore(client,
		WithTwoLevelLogger(logger),
		WithTwoLevelInvalidationBroadcast(client, "gincache:test:bad-payload"),
	)
	t.Cleanup(func() {
		_ = store.Close()
	})
	waitForSubscribers(t, mini, "gincache:test:bad-payload", 1)

	if err := client.Publish(context.Background(), "gincache:test:bad-payload", "{bad json").Err(); err != nil {
		t.Fatalf("Publish bad payload error: %v", err)
	}

	logger.waitFor(t, "invalid invalidation message")
}

func TestTwoLevelStoreInvalidationBroadcastLogsPublishFailure(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
	})

	logger := &captureLogger{}
	store := NewTwoLevelStore(client,
		WithTwoLevelLogger(logger),
		WithTwoLevelInvalidationBroadcast(client, "gincache:test:publish-fail"),
	)
	t.Cleanup(func() {
		_ = store.Close()
	})
	waitForSubscribers(t, mini, "gincache:test:publish-fail", 1)

	mini.Close()
	store.publishInvalidation(context.Background(), twoLevelInvalidationKey, "route:/products")

	logger.waitFor(t, "failed to publish invalidation")
}

func TestTwoLevelStoreInvalidationBroadcastLogsLocalDeleteFailure(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	clientA := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	clientB := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() {
		_ = clientA.Close()
		_ = clientB.Close()
	})

	logger := &captureLogger{}
	storeA := NewTwoLevelStore(clientA,
		WithTwoLevelLogger(logger),
		WithLocalStore(failingDeleteLocalStore{}),
		WithTwoLevelInvalidationBroadcast(clientA, "gincache:test:delete-fail"),
	)
	storeB := NewTwoLevelStore(clientB,
		WithTwoLevelInvalidationBroadcast(clientB, "gincache:test:delete-fail"),
	)
	t.Cleanup(func() {
		_ = storeA.Close()
		_ = storeB.Close()
	})
	waitForSubscribers(t, mini, "gincache:test:delete-fail", 2)

	storeB.publishInvalidation(context.Background(), twoLevelInvalidationKey, "route:/products")

	logger.waitFor(t, "failed to delete local key")
}

func TestTwoLevelStoreInvalidationBroadcastLogsSubscribeFailure(t *testing.T) {
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

	logger := &captureLogger{}
	store := NewTwoLevelStore(client,
		WithTwoLevelLogger(logger),
		WithTwoLevelInvalidationBroadcast(client, "gincache:test:subscribe-fail"),
		WithTwoLevelInvalidationTimeout(50*time.Millisecond),
	)
	t.Cleanup(func() {
		_ = store.Close()
	})

	if store.invalidation != nil {
		t.Fatal("invalidation should be disabled after subscribe failure")
	}
	logger.waitFor(t, "failed to start invalidation subscriber")
}

func TestTwoLevelStoreDeletePatternAndLocalInvalidationHelpers(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
	})

	store := NewTwoLevelStore(client,
		WithLocalTTL(time.Minute),
		WithRemoteTTL(time.Minute),
		WithTwoLevelKeyPrefix("test:2l:"),
	)
	t.Cleanup(func() {
		_ = store.Close()
	})

	for key, value := range map[string]map[string]any{
		"user:1":  {"v": 1},
		"user:2":  {"v": 2},
		"order:1": {"v": 3},
	} {
		if err := store.Set(key, value, time.Minute); err != nil {
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

	store.InvalidateLocal("order:1")
	eventually(t, time.Second, func() bool {
		var cached map[string]any
		return errors.Is(store.local.Get("order:1", &cached), ErrCacheMiss)
	})

	if err := store.Get("order:1", &got); err != nil {
		t.Fatalf("Get order:1 after local invalidation error: %v", err)
	}
	store.InvalidateLocalPattern("order:*")
	eventually(t, time.Second, func() bool {
		var cached map[string]any
		return errors.Is(store.local.Get("order:1", &cached), ErrCacheMiss)
	})

	stats := store.Stats()
	if stats["remote_hit"] == 0 {
		t.Fatalf("remote_hit = %d, want > 0", stats["remote_hit"])
	}
	store.ResetStats()
	stats = store.Stats()
	if stats["remote_hit"] != 0 || stats["local_hit"] != 0 || stats["miss"] != 0 {
		t.Fatalf("stats after reset = %#v, want zero hit counters", stats)
	}

	if store.RemoteStore() == nil {
		t.Fatal("RemoteStore returned nil")
	}
}

func TestTwoLevelStoreInvalidationBroadcastIgnoresEmptyConfig(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
	})

	store := NewTwoLevelStore(client,
		WithTwoLevelInvalidationBroadcast(nil, "gincache:test:nil-client"),
		WithTwoLevelInvalidationBroadcast(client, ""),
	)
	t.Cleanup(func() {
		_ = store.Close()
	})

	if store.invalidation != nil {
		t.Fatal("invalidation should stay disabled for empty broadcast config")
	}
}

func TestTwoLevelStoreInvalidationBroadcastLogsUnknownType(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
	})

	logger := &captureLogger{}
	store := NewTwoLevelStore(client,
		WithTwoLevelLogger(logger),
		WithTwoLevelInvalidationBroadcast(client, "gincache:test:unknown-type"),
	)
	t.Cleanup(func() {
		_ = store.Close()
	})
	waitForSubscribers(t, mini, "gincache:test:unknown-type", 1)

	payload, err := json.Marshal(twoLevelInvalidationMessage{
		Origin: "external",
		Type:   "unknown",
		Value:  "route:/products",
	})
	if err != nil {
		t.Fatalf("Marshal invalidation message error: %v", err)
	}
	if err := client.Publish(context.Background(), "gincache:test:unknown-type", payload).Err(); err != nil {
		t.Fatalf("Publish unknown type error: %v", err)
	}

	logger.waitFor(t, "unknown invalidation message type")
}

type blockingLocalStore struct {
	mu      sync.Mutex
	values  map[string][]byte
	gets    atomic.Int64
	blocked chan struct{}
}

func newBlockingLocalStore() *blockingLocalStore {
	return &blockingLocalStore{
		values:  make(map[string][]byte),
		blocked: make(chan struct{}),
	}
}

func (s *blockingLocalStore) Get(key string, value any) error {
	n := s.gets.Add(1)
	if n == 2 {
		<-s.blocked
	}

	s.mu.Lock()
	data, ok := s.values[key]
	s.mu.Unlock()
	if !ok {
		return ErrCacheMiss
	}

	return json.Unmarshal(data, value)
}

func (s *blockingLocalStore) Set(key string, value any, _ time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.values[key] = data
	s.mu.Unlock()
	return nil
}

func (s *blockingLocalStore) Delete(key string) error {
	s.mu.Lock()
	delete(s.values, key)
	s.mu.Unlock()
	return nil
}

func (s *blockingLocalStore) Close() error {
	select {
	case <-s.blocked:
	default:
		close(s.blocked)
	}
	return nil
}

func (s *blockingLocalStore) Stats() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]int64{"keys": int64(len(s.values))}
}

func (s *blockingLocalStore) ResetStats() {}

var errLocalSetFailed = errors.New("local set failed")

type failingSetLocalStore struct{}

func (failingSetLocalStore) Get(string, any) error {
	return ErrCacheMiss
}

func (failingSetLocalStore) Set(string, any, time.Duration) error {
	return errLocalSetFailed
}

func (failingSetLocalStore) Delete(string) error {
	return nil
}

func (failingSetLocalStore) Close() error {
	return nil
}

func (failingSetLocalStore) Stats() map[string]int64 {
	return map[string]int64{"keys": 0}
}

func (failingSetLocalStore) ResetStats() {}

var errLocalDeleteFailed = errors.New("local delete failed")

type failingDeleteLocalStore struct{}

func (failingDeleteLocalStore) Get(string, any) error {
	return ErrCacheMiss
}

func (failingDeleteLocalStore) Set(string, any, time.Duration) error {
	return nil
}

func (failingDeleteLocalStore) Delete(string) error {
	return errLocalDeleteFailed
}

func (failingDeleteLocalStore) Close() error {
	return nil
}

func (failingDeleteLocalStore) Stats() map[string]int64 {
	return map[string]int64{"keys": 0}
}

func (failingDeleteLocalStore) ResetStats() {}

type captureLogger struct {
	mu      sync.Mutex
	entries []string
}

func (l *captureLogger) Errorf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, fmt.Sprintf(format, args...))
}

func (l *captureLogger) contains(part string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entry := range l.entries {
		if strings.Contains(entry, part) {
			return true
		}
	}
	return false
}

func (l *captureLogger) waitFor(t *testing.T, part string) {
	t.Helper()
	eventually(t, time.Second, func() bool {
		return l.contains(part)
	})
}

func waitForSubscribers(t *testing.T, mini *miniredis.Miniredis, channel string, want int) {
	t.Helper()
	eventually(t, time.Second, func() bool {
		return mini.PubSubNumSub(channel)[channel] >= want
	})
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if condition() {
		return
	}

	t.Fatal("condition did not become true before timeout")
}
