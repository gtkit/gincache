package persist

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/gtkit/json"
	"github.com/redis/go-redis/v9"
)

// TestTwoLevelStoreSetReportsOnlyRemoteResult 钉住本地失效失败不混入 Set 的返回值。
//
// 返回值表达的是 Redis 的写入结果——Redis 已经是新值，这次写确实成功了。把本地
// 失败报成错误会让调用方去重试一次已经成功的写入。与 Delete 的约定一致。
// 本地失效失败时 L1 会残留旧值，最多活到 localTTL。
func TestTwoLevelStoreSetReportsOnlyRemoteResult(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	local := newStaleLocalStore()
	store := NewTwoLevelStore(client,
		WithLocalTTL(time.Minute),
		WithRemoteTTL(time.Minute),
		WithLocalStore(local),
	)
	t.Cleanup(func() { _ = store.Close() })

	// L1 里先有一份旧值，模拟这个 key 之前被正常缓存过。
	local.seed(t, "route:/products", map[string]any{"version": "v1"})

	// 写路径失效 L1（不再写 L1），因此这里让本地删除失败。
	local.failDelete(errLocalDeleteFailed)
	err := store.Set("route:/products", map[string]any{"version": "v2"}, time.Minute)
	if err != nil {
		t.Fatalf("Set error = %v, want nil——本地失效失败只记日志，Redis 写已成功", err)
	}

	// 本地失效失败时旧值会残留，最多活到 localTTL；恢复后失效并确认读到新值。
	local.failDelete(nil)
	store.InvalidateLocal("route:/products")

	var got map[string]any
	if err := store.Get("route:/products", &got); err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if got["version"] != "v2" {
		t.Fatalf("Get version = %v, want v2（读到了失效前的旧值）", got["version"])
	}
}

// TestTwoLevelStoreInvalidatesLocalOnSuccessfulSet 钉住写路径失效 L1 而不是写入 L1。
//
// 这里此前断言的是"Set 成功后 L1 保留新值、且不调用本地删除"。写 L1 会让两级的
// 最终值可能相反：远端写在守卫临界区之外，两个并发 Set 的远端写顺序与 L1 写顺序
// 可以颠倒。改为失效之后，Redis 留下胜者，下次读从 Redis 回填，两级必然一致。
func TestTwoLevelStoreInvalidatesLocalOnSuccessfulSet(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	local := newStaleLocalStore()
	store := NewTwoLevelStore(client,
		WithLocalTTL(time.Minute),
		WithRemoteTTL(time.Minute),
		WithLocalStore(local),
	)
	t.Cleanup(func() { _ = store.Close() })

	local.seed(t, "route:/products", map[string]any{"version": "v0"})

	if err := store.Set("route:/products", map[string]any{"version": "v1"}, time.Minute); err != nil {
		t.Fatalf("Set error: %v", err)
	}

	if local.has("route:/products") {
		t.Fatal("Set 之后 L1 仍持有值——写路径应当失效 L1")
	}

	// 下一次读从 Redis 回填，拿到的是刚写入的值。
	var got map[string]any
	if err := store.Get("route:/products", &got); err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if got["version"] != "v1" {
		t.Fatalf("Get version = %v, want v1", got["version"])
	}
	if !local.has("route:/products") {
		t.Fatal("读之后 L1 应已被回填")
	}
}

// TestTwoLevelStoreLogsLocalDeleteFailure 钉住本地删除失败被记录且不改变返回值。
//
// 返回值表达的是 Redis 的删除结果；把本地失败混进去会让调用方误判 Redis 状态，
// 而且某些本地实现删除不存在的 key 本就返回良性错误。
func TestTwoLevelStoreLogsLocalDeleteFailure(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	logger := &captureLogger{}
	store := NewTwoLevelStore(client,
		WithTwoLevelLogger(logger),
		WithLocalStore(failingPatternLocalStore{}),
	)
	t.Cleanup(func() { _ = store.Close() })

	if err := store.Delete("route:/products"); err != nil {
		t.Fatalf("Delete error = %v, want nil（本地失败不应混入返回值）", err)
	}
	if !logger.contains("failed to delete local key") {
		t.Fatal("本地删除失败没有被记录")
	}

	if _, err := store.DeletePattern(context.Background(), "route:*"); err != nil {
		t.Fatalf("DeletePattern error = %v, want nil（本地失败不应混入返回值）", err)
	}
	if !logger.contains("failed to delete local pattern") {
		t.Fatal("本地按模式删除失败没有被记录")
	}
}

// TestTwoLevelStoreCountsHitSource 钉住命中来源统计区分 L1 与 L2。
//
// singleflight 内第二次本地检查命中时，此前仍无条件计入 remoteHit，
// 并发回填期间的 L1 命中会被算成 L2 命中，local_hit_rate 因此失真。
func TestTwoLevelStoreCountsHitSource(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	local := newStaleLocalStore()
	store := NewTwoLevelStore(client,
		WithLocalTTL(time.Minute),
		WithRemoteTTL(time.Minute),
		WithLocalStore(local),
	)
	t.Cleanup(func() { _ = store.Close() })

	// 只写 Redis，让 L1 保持为空，第一次读必然回源。
	if err := store.RemoteStore().Set("route:/products", map[string]any{"v": 1}, time.Minute); err != nil {
		t.Fatalf("remote Set error: %v", err)
	}

	var got map[string]any
	if err := store.Get("route:/products", &got); err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if stats := store.Stats(); stats["remote_hit"] != 1 || stats["local_hit"] != 0 {
		t.Fatalf("回源后 remote_hit=%d local_hit=%d, want 1/0", stats["remote_hit"], stats["local_hit"])
	}

	// 回源时已回填 L1，第二次读走最外层的本地命中。
	if err := store.Get("route:/products", &got); err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if stats := store.Stats(); stats["remote_hit"] != 1 || stats["local_hit"] != 1 {
		t.Fatalf("本地命中后 remote_hit=%d local_hit=%d, want 1/1", stats["remote_hit"], stats["local_hit"])
	}

	// 构造 singleflight 内的第二次本地检查命中：最外层读不到、内层读得到。
	store.ResetStats()
	local.failNextGet()
	if err := store.Get("route:/products", &got); err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if stats := store.Stats(); stats["local_hit"] != 1 || stats["remote_hit"] != 0 {
		t.Fatalf("singleflight 内本地命中后 local_hit=%d remote_hit=%d, want 1/0", stats["local_hit"], stats["remote_hit"])
	}
}

// TestNewTwoLevelStoreValidatesConfiguration 钉住配置错误在构造期而不是首次读写时暴露。
func TestNewTwoLevelStoreValidatesConfiguration(t *testing.T) {
	t.Parallel()

	t.Run("nil client 构造期 panic", func(t *testing.T) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				t.Fatal("nil client 没有 panic")
			}
			if msg, _ := recovered.(string); msg != "gincache: redisClient must not be nil" {
				t.Fatalf("panic 信息 = %v，未指明 client 不能为 nil", recovered)
			}
		}()
		NewTwoLevelStore(nil)
	})

	t.Run("nil Option 被跳过且负数 TTL 被忽略", func(t *testing.T) {
		mini := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
		t.Cleanup(func() { _ = client.Close() })

		store := NewTwoLevelStore(client, nil, WithLocalTTL(-time.Second), nil, WithRemoteTTL(-time.Minute))
		t.Cleanup(func() { _ = store.Close() })

		if store.localTTL != 30*time.Second {
			t.Fatalf("localTTL = %v, want 30s（负数应被忽略）", store.localTTL)
		}
		if store.remoteTTL != 5*time.Minute {
			t.Fatalf("remoteTTL = %v, want 5m（负数应被忽略）", store.remoteTTL)
		}
	})
}

// staleLocalStore 是一个可控制写入/删除失败、并保留旧值的本地缓存桩。
type staleLocalStore struct {
	mu        sync.Mutex
	values    map[string][]byte
	deleteErr error
	deletes   int
	skipNext  bool
}

func newStaleLocalStore() *staleLocalStore {
	return &staleLocalStore{values: make(map[string][]byte)}
}

func (s *staleLocalStore) seed(t *testing.T, key string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("seed marshal error: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = data
}

func (s *staleLocalStore) failDelete(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteErr = err
}

func (s *staleLocalStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.values[key]
	return ok
}

// failNextGet 让下一次 Get 谎报未命中，用于构造"最外层读不到、singleflight
// 内层读得到"这条路径。
func (s *staleLocalStore) failNextGet() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skipNext = true
}

func (s *staleLocalStore) Get(key string, value any) error {
	s.mu.Lock()
	if s.skipNext {
		s.skipNext = false
		s.mu.Unlock()
		return ErrCacheMiss
	}
	data, ok := s.values[key]
	s.mu.Unlock()
	if !ok {
		return ErrCacheMiss
	}
	return json.Unmarshal(data, value)
}

func (s *staleLocalStore) Set(key string, value any, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.values[key] = data
	return nil
}

func (s *staleLocalStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.values, key)
	return nil
}

func (s *staleLocalStore) Close() error { return nil }

func (s *staleLocalStore) Stats() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]int64{"keys": int64(len(s.values))}
}

func (s *staleLocalStore) ResetStats() {}

var errLocalPatternDeleteFailed = errors.New("local pattern delete failed")

// failingPatternLocalStore 的单 key 删除和按模式删除都失败，用于验证两条日志路径。
type failingPatternLocalStore struct{}

func (failingPatternLocalStore) Get(string, any) error {
	return ErrCacheMiss
}

func (failingPatternLocalStore) Set(string, any, time.Duration) error {
	return nil
}

func (failingPatternLocalStore) Delete(string) error {
	return errLocalDeleteFailed
}

func (failingPatternLocalStore) DeletePattern(context.Context, string) (int64, error) {
	return 0, errLocalPatternDeleteFailed
}

func (failingPatternLocalStore) Close() error {
	return nil
}

func (failingPatternLocalStore) Stats() map[string]int64 {
	return map[string]int64{"keys": 0}
}

func (failingPatternLocalStore) ResetStats() {}
