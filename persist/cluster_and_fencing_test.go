package persist

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestMemoryStoreExpiryKeepsConcurrentWrite 钉住过期删除只删自己读到的条目。
//
// Load 与删除之间允许并发 Set 换上新值，无条件 Delete 会把刚写入的新值一起删掉。
// 改前实测 3000 轮中 Get 路径丢 112 次、后台清理路径丢 127 次。
func TestMemoryStoreExpiryKeepsConcurrentWrite(t *testing.T) {
	t.Parallel()

	// expire 触发过期删除的两条路径：Get 内的惰性删除，以及后台清理。
	paths := map[string]func(*MemoryStore){
		"Get 惰性删除": func(s *MemoryStore) {
			var out map[string]string
			_ = s.Get("k", &out)
		},
		"后台清理": func(s *MemoryStore) { s.deleteExpired() },
	}

	for name, expire := range paths {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			const rounds = 2000
			var lost int

			for range rounds {
				// cleanupInterval 设大，排除后台协程干扰，只测被点名的那条路径。
				s := NewMemoryStore(time.Hour, WithCleanupInterval(time.Hour))

				// 放一个刚刚过期的条目。
				s.data.Store("k", &memoryItem{
					value:      []byte(`{"v":"old"}`),
					expiration: time.Now().Add(-time.Nanosecond).UnixNano(),
				})

				var wg sync.WaitGroup
				start := make(chan struct{})

				wg.Go(func() {
					<-start
					expire(s)
				})
				wg.Go(func() {
					<-start
					_ = s.Set("k", map[string]string{"v": "new"}, time.Hour)
				})

				close(start)
				wg.Wait()

				// 写者返回成功之后，新值必须还在。
				var out map[string]string
				if err := s.Get("k", &out); errors.Is(err, ErrCacheMiss) {
					lost++
				}
				_ = s.Close()
			}

			if lost > 0 {
				t.Fatalf("%d 轮中有 %d 次并发写入被过期删除吃掉", rounds, lost)
			}
		})
	}
}

// TestMemoryStoreStillReclaimsExpired 钉住 CompareAndDelete 没有妨碍正常回收。
func TestMemoryStoreStillReclaimsExpired(t *testing.T) {
	t.Parallel()

	s := NewMemoryStore(time.Hour, WithCleanupInterval(time.Hour))
	t.Cleanup(func() { _ = s.Close() })

	s.data.Store("expired", &memoryItem{
		value:      []byte(`{"v":"old"}`),
		expiration: time.Now().Add(-time.Second).UnixNano(),
	})
	if err := s.Set("alive", map[string]string{"v": "1"}, time.Hour); err != nil {
		t.Fatal(err)
	}

	s.deleteExpired()

	if _, ok := s.data.Load("expired"); ok {
		t.Fatal("过期条目没有被回收")
	}
	if _, ok := s.data.Load("alive"); !ok {
		t.Fatal("未过期条目被误删")
	}
}

// TestTwoLevelStoreConcurrentSetKeepsLevelsAgreeing 钉住并发变更后两级最终值一致。
//
// 远端写在守卫临界区之外，两个并发 Set 的远端写顺序与 L1 写顺序可以颠倒。改前实测：
// Redis=B 而 L1=A，两个调用都返回成功。写路径改为失效 L1 之后不再可能。
func TestTwoLevelStoreConcurrentSetKeepsLevelsAgreeing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// second 是在 A 的远端写完成后、A 写 L1 之前跑完的那个操作。
		second func(*TwoLevelStore) error
		// wantRemote 为空表示 Redis 应当已无该 key。
		wantRemote string
	}{
		{
			name: "两个并发 Set",
			second: func(s *TwoLevelStore) error {
				return s.Set("k", map[string]string{"v": "B"}, time.Minute)
			},
			wantRemote: "B",
		},
		{
			name: "Set 与 Delete 混合并发",
			second: func(s *TwoLevelStore) error {
				return s.Delete("k")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mini := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
			t.Cleanup(func() { _ = client.Close() })

			store := NewTwoLevelStore(client, WithLocalTTL(time.Minute), WithRemoteTTL(time.Minute))
			t.Cleanup(func() { _ = store.Close() })

			aRemoteDone := make(chan struct{})
			releaseA := make(chan struct{})
			var first atomic.Bool

			// 钉死顺序：A 的远端写完成 → 第二个操作全程跑完 → A 才继续。
			client.AddHook(afterCommandHook{after: func(cmd redis.Cmder) {
				if cmd.Name() != "set" {
					return
				}
				if first.CompareAndSwap(false, true) {
					close(aRemoteDone)
					<-releaseA
				}
			}})

			var wg sync.WaitGroup
			wg.Go(func() {
				if err := store.Set("k", map[string]string{"v": "A"}, time.Minute); err != nil {
					t.Errorf("Set A: %v", err)
				}
			})

			<-aRemoteDone
			if err := tc.second(store); err != nil {
				t.Fatalf("second op: %v", err)
			}
			close(releaseA)
			wg.Wait()

			var remote map[string]string
			remoteErr := store.RemoteStore().Get("k", &remote)

			var got map[string]string
			readErr := store.Get("k", &got)

			if tc.wantRemote == "" {
				if remoteErr == nil {
					t.Fatalf("Redis 应已无该 key，实际 %v", remote)
				}
				if readErr == nil {
					t.Fatalf("读到了 Redis 已不持有的值 %v", got)
				}
				return
			}

			if remoteErr != nil {
				t.Fatalf("Redis 读取失败: %v", remoteErr)
			}
			if readErr != nil {
				t.Fatalf("读取失败: %v", readErr)
			}
			if got["v"] != remote["v"] {
				t.Fatalf("两级不一致：读到 %q 而 Redis=%q", got["v"], remote["v"])
			}
			if remote["v"] != tc.wantRemote {
				t.Fatalf("Redis = %q, want %q", remote["v"], tc.wantRemote)
			}
		})
	}
}

// TestTwoLevelStoreSupersededCheckSharesCriticalSection 钉住淘汰标记在临界区内判定。
//
// 两个同 key leader 在超时释放后并发、且期间无变更时代次相同：若标记在临界区外先查
// 一次，两个回填都会通过校验，旧的最后落地。
func TestTwoLevelStoreSupersededCheckSharesCriticalSection(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := NewTwoLevelStore(client, WithLocalTTL(time.Minute), WithRemoteTTL(time.Minute))
	t.Cleanup(func() { _ = store.Close() })

	guard := store.guard("k")
	generation := guard.begin()

	// 模拟"旧 leader 已通过代次校验、随后被超时释放"：标记置位后 commitRead 必须拒绝。
	var superseded atomic.Bool
	superseded.Store(true)

	wrote := guard.commitRead(generation, &superseded, func() {
		_ = store.local.Set("k", map[string]string{"v": "stale"}, time.Minute)
	})
	if wrote {
		t.Fatal("被淘汰的 leader 通过了 commitRead——标记没有在临界区内判定")
	}

	var l1 map[string]string
	if err := store.local.Get("k", &l1); err == nil {
		t.Fatalf("L1 有残留 %v", l1)
	}

	// 未被淘汰且代次未变时仍应写入。
	var fresh atomic.Bool
	if !guard.commitRead(generation, &fresh, func() {
		_ = store.local.Set("k", map[string]string{"v": "ok"}, time.Minute)
	}) {
		t.Fatal("未被淘汰的 leader 被拒绝了")
	}
}

// TestRedisStoreDeleteKeysAcrossSlots 钉住批量删除逐 key 下发。
//
// 一条 DEL 带多个 key 时 Redis Cluster 要求同 slot，go-redis 按首个 key 的 slot 路由且
// 不拆分，跨 slot 会得到 CROSSSLOT。这里用单节点验证语义与计数正确，跨节点覆盖见
// Ring 用例。
func TestRedisStoreDeleteKeysAcrossSlots(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := NewRedisStore(client, WithKeyPrefix("t:"))

	keys := []string{"a", "b", "c"}
	for _, k := range keys {
		if err := store.Set(k, map[string]string{"v": k}, time.Minute); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.DeleteKeys(context.Background(), keys...); err != nil {
		t.Fatalf("DeleteKeys: %v", err)
	}

	for _, k := range keys {
		var out map[string]string
		if err := store.Get(k, &out); !errors.Is(err, ErrCacheMiss) {
			t.Fatalf("key %q 未被删除，err = %v", k, err)
		}
	}

	t.Run("空集合是 no-op", func(t *testing.T) {
		if err := store.DeleteKeys(context.Background()); err != nil {
			t.Fatalf("空集合应当成功: %v", err)
		}
	})
}

// TestRedisStoreRingFansOutAcrossShards 钉住分片型客户端下按模式删除与统计覆盖全部节点。
//
// go-redis 的 Ring 与 ClusterClient 都没有覆写 Scan：无 key 命令只会落到一个节点。
// 用多个 miniredis 组成 Ring 可以真实覆盖扇出逻辑。
//
// 注意：*redis.ClusterClient 分支与这里结构对称，但本地没有真集群，该分支未被测试覆盖。
func TestRedisStoreRingFansOutAcrossShards(t *testing.T) {
	t.Parallel()

	shardA := miniredis.RunT(t)
	shardB := miniredis.RunT(t)

	ring := redis.NewRing(&redis.RingOptions{
		Addrs: map[string]string{
			"a": shardA.Addr(),
			"b": shardB.Addr(),
		},
	})
	t.Cleanup(func() { _ = ring.Close() })

	store := NewRedisStore(ring, WithKeyPrefix("ring:"))

	// 写足够多的 key，确保两个 shard 上都落到数据。
	const total = 64
	for i := range total {
		if err := store.Set(fmt.Sprintf("k%d", i), map[string]int{"i": i}, time.Minute); err != nil {
			t.Fatal(err)
		}
	}

	countedA := len(shardA.Keys())
	countedB := len(shardB.Keys())
	if countedA == 0 || countedB == 0 {
		t.Skipf("key 未分散到两个 shard（a=%d b=%d），无法验证扇出", countedA, countedB)
	}

	if got := store.Stats()["keys"]; got != int64(total) {
		t.Fatalf("Stats keys = %d, want %d（a=%d b=%d）——统计只覆盖了部分 shard",
			got, total, countedA, countedB)
	}

	deleted, err := store.DeletePattern(context.Background(), "k*")
	if err != nil {
		t.Fatalf("DeletePattern: %v", err)
	}
	if deleted != int64(total) {
		t.Fatalf("DeletePattern 删除 %d 个, want %d——只覆盖了部分 shard", deleted, total)
	}

	if remainA, remainB := len(shardA.Keys()), len(shardB.Keys()); remainA != 0 || remainB != 0 {
		t.Fatalf("仍有残留：a=%d b=%d", remainA, remainB)
	}
}

// afterCommandHook 在单条命令执行成功后插入回调，用于把并发窗口钉成确定顺序。
type afterCommandHook struct {
	after func(redis.Cmder)
}

func (afterCommandHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h afterCommandHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if err == nil {
			h.after(cmd)
		}
		return err
	}
}

func (afterCommandHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// TestRedisStoreScanBatchSizePerPath 钉住两条扫描路径各自的批量大小。
//
// 抽出公共的 scanNode 时曾把 Stats 原本的 COUNT 1000 一起统一成删除路径的 100，
// 让统计的 SCAN 往返翻了十倍——而 Stats 还要遍历整个前缀、分片客户端下遍历每个节点。
// 统一常量很容易再犯，这里按路径分别钉住。
func TestRedisStoreScanBatchSizePerPath(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := NewRedisStore(client, WithKeyPrefix("scan:"))

	const total = 1500
	for i := range total {
		if err := store.Set(fmt.Sprintf("k%d", i), i, time.Minute); err != nil {
			t.Fatal(err)
		}
	}

	var scans atomic.Int64
	client.AddHook(scanCountHook{n: &scans})

	if got := store.Stats()["keys"]; got != total {
		t.Fatalf("Stats keys = %d, want %d", got, total)
	}

	// COUNT=1000 时约 2 次；若退回 100 会是 8 次以上。留出余量但保持区分度。
	statsScans := scans.Load()
	if maxScans := int64(total/statsScanCount) + 2; statsScans > maxScans {
		t.Fatalf("Stats 用了 %d 次 SCAN，超过 %d——统计路径的批量大小被改小了", statsScans, maxScans)
	}

	scans.Store(0)
	if _, err := store.DeletePattern(context.Background(), "k*"); err != nil {
		t.Fatalf("DeletePattern: %v", err)
	}
	if scans.Load() == 0 {
		t.Fatal("删除路径没有发出 SCAN")
	}
}

// scanCountHook 统计发出的 SCAN 命令次数。
type scanCountHook struct{ n *atomic.Int64 }

func (scanCountHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h scanCountHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "scan" {
			h.n.Add(1)
		}
		return next(ctx, cmd)
	}
}

func (scanCountHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
