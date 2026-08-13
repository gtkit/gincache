package persist

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// localExpirationOf 读取 L1 条目的过期时间戳；0 表示永不过期，-1 表示条目不存在。
func localExpirationOf(t *testing.T, store *TwoLevelStore, key string) int64 {
	t.Helper()

	memory, ok := store.local.(*MemoryStore)
	if !ok {
		t.Fatalf("local store 类型 = %T，want *MemoryStore", store.local)
	}
	value, ok := memory.data.Load(key)
	if !ok {
		return -1
	}
	return value.(*memoryItem).expiration
}

// TestNewTwoLevelStoreRejectsZeroLocalTTL 钉住零值 localTTL 的拒绝。
//
// 零值此前被显式放过（校验写的是 ttl >= 0），一路传到 MemoryStore 得到
// expiration=0：条目永不过期，而后台清理只回收 expiration > 0 的条目，于是它也
// 永不被回收。Redis 条目过期后本实例永久返回旧值，L1 同时失去唯一的回收机制。
func TestNewTwoLevelStoreRejectsZeroLocalTTL(t *testing.T) {
	t.Parallel()

	t.Run("零值 localTTL 构造期 panic 并指出两条出路", func(t *testing.T) {
		t.Parallel()

		mini := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
		t.Cleanup(func() { _ = client.Close() })

		defer func() {
			recovered := recover()
			if recovered == nil {
				t.Fatal("零值 localTTL 没有 panic")
			}
			msg, _ := recovered.(string)
			for _, want := range []string{"localTTL must not be zero", "omit WithLocalTTL", "positive duration"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("panic 信息 = %q，缺少 %q", msg, want)
				}
			}
		}()
		NewTwoLevelStore(client, WithLocalTTL(0))
	})

	t.Run("校验的是最终生效值", func(t *testing.T) {
		t.Parallel()

		mini := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
		t.Cleanup(func() { _ = client.Close() })

		// 先 0 后正数：不该误报，最终值才是生效值。
		store := NewTwoLevelStore(client, WithLocalTTL(0), WithLocalTTL(time.Minute))
		t.Cleanup(func() { _ = store.Close() })

		if store.localTTL != time.Minute {
			t.Fatalf("localTTL = %v, want 1m", store.localTTL)
		}
	})

	t.Run("默认构造仍然有效且回填的 L1 条目会过期", func(t *testing.T) {
		t.Parallel()

		mini := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
		t.Cleanup(func() { _ = client.Close() })

		store := NewTwoLevelStore(client)
		t.Cleanup(func() { _ = store.Close() })

		if err := store.Set("k", map[string]string{"v": "1"}, time.Minute); err != nil {
			t.Fatal(err)
		}
		// 写路径失效 L1 而不写入，因此要先读一次触发回填，再检查过期时间。
		var out map[string]string
		if err := store.Get("k", &out); err != nil {
			t.Fatal(err)
		}
		if got := localExpirationOf(t, store, "k"); got <= 0 {
			t.Fatalf("L1 expiration = %d，want 正值（0 表示永不过期）", got)
		}
	})
}

// TestLocalBackfillTTL 钉住回填时长的三态决策。
//
// PTTL 的两个哨兵值 go-redis 直接返回 time.Duration(n) 而不乘精度，所以判断必须
// 精确比较，不能按毫秒推算。
func TestLocalBackfillTTL(t *testing.T) {
	t.Parallel()

	const localTTL = 30 * time.Second

	tests := []struct {
		name      string
		remaining time.Duration
		wantTTL   time.Duration
		wantOK    bool
	}{
		{"远端剩余短于 localTTL 时取远端", time.Second, time.Second, true},
		{"远端剩余长于 localTTL 时取 localTTL", 5 * time.Minute, localTTL, true},
		{"远端剩余等于 localTTL", localTTL, localTTL, true},
		{"远端无过期时间用完整 localTTL", remoteTTLNoExpiry, localTTL, true},
		{"远端条目已消失则不回填", remoteTTLKeyMissing, 0, false},
		{"非正剩余时间一律不回填", 0, 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotTTL, gotOK := localBackfillTTL(localTTL, tc.remaining)
			if gotOK != tc.wantOK || gotTTL != tc.wantTTL {
				t.Fatalf("localBackfillTTL(%v, %v) = (%v, %v), want (%v, %v)",
					localTTL, tc.remaining, gotTTL, gotOK, tc.wantTTL, tc.wantOK)
			}
			if gotOK && gotTTL <= 0 {
				t.Fatalf("回填 TTL = %v，必须为正数——传 0 给 LocalStore 会退回它自己的默认值", gotTTL)
			}
		})
	}
}

// TestRedisStoreGetWithTTLStates 钉住一次往返取值加剩余 TTL 的三态返回。
func TestRedisStoreGetWithTTLStates(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := NewRedisStore(client, WithKeyPrefix("t:"))

	if err := store.Set("withttl", map[string]string{"v": "1"}, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("nottl", map[string]string{"v": "2"}, 0); err != nil {
		t.Fatal(err)
	}

	t.Run("有过期时间返回正的剩余值", func(t *testing.T) {
		var out map[string]string
		remaining, err := store.getWithTTL("withttl", &out)
		if err != nil {
			t.Fatal(err)
		}
		if remaining <= 0 || remaining > 30*time.Second {
			t.Fatalf("remaining = %v，want (0, 30s]", remaining)
		}
		if out["v"] != "1" {
			t.Fatalf("value = %v, want v=1", out)
		}
	})

	t.Run("无过期时间返回哨兵值", func(t *testing.T) {
		var out map[string]string
		remaining, err := store.getWithTTL("nottl", &out)
		if err != nil {
			t.Fatal(err)
		}
		if remaining != remoteTTLNoExpiry {
			t.Fatalf("remaining = %d, want %d", remaining, remoteTTLNoExpiry)
		}
		if out["v"] != "2" {
			t.Fatalf("value = %v, want v=2", out)
		}
	})

	t.Run("key 不存在返回 ErrCacheMiss", func(t *testing.T) {
		var out map[string]string
		if _, err := store.getWithTTL("missing", &out); !errors.Is(err, ErrCacheMiss) {
			t.Fatalf("err = %v, want ErrCacheMiss", err)
		}
	})
}

// TestTwoLevelStoreBackfillBoundedByRemoteRemainingTTL 钉住"L1 条目不得活过 L2 条目"。
//
// 回填此前固定用完整 localTTL：一个只剩 1 秒的远端条目能在 L1 里再活 30 秒，
// 事实来源过期之后的 29 秒仍然返回它。写路径早已按 min 约束，回填路径漏了。
func TestTwoLevelStoreBackfillBoundedByRemoteRemainingTTL(t *testing.T) {
	t.Parallel()

	t.Run("回填受远端剩余 TTL 钳制", func(t *testing.T) {
		t.Parallel()

		mini := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
		t.Cleanup(func() { _ = client.Close() })

		store := NewTwoLevelStore(client, WithLocalTTL(30*time.Second), WithRemoteTTL(time.Minute))
		t.Cleanup(func() { _ = store.Close() })

		// 只写远端，且远端剩余远短于 localTTL；L1 为空。
		const remoteRemaining = 200 * time.Millisecond
		if err := store.RemoteStore().Set("k", map[string]string{"v": "x"}, remoteRemaining); err != nil {
			t.Fatal(err)
		}

		var out map[string]string
		if err := store.Get("k", &out); err != nil {
			t.Fatalf("应命中 L2: %v", err)
		}

		remaining := time.Until(time.Unix(0, localExpirationOf(t, store, "k")))
		if remaining > remoteRemaining {
			t.Fatalf("L1 剩余 TTL = %v，超过远端剩余的 %v——L1 会活过 L2", remaining, remoteRemaining)
		}

		// 两级用的是不同的时钟：miniredis 的 TTL 只随 FastForward 前进，而 L1 的
		// MemoryStore 走真实墙钟。要观察"远端过期后 L1 也不再供数"，两个都得推进。
		mini.FastForward(2 * remoteRemaining)
		time.Sleep(remoteRemaining + 50*time.Millisecond)

		if err := store.Get("k", &out); !errors.Is(err, ErrCacheMiss) {
			t.Fatalf("远端过期后 Get err = %v, want ErrCacheMiss", err)
		}
	})

	t.Run("远端无过期时间时用完整 localTTL", func(t *testing.T) {
		t.Parallel()

		mini := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
		t.Cleanup(func() { _ = client.Close() })

		const localTTL = 30 * time.Second
		store := NewTwoLevelStore(client, WithLocalTTL(localTTL), WithRemoteTTL(time.Minute))
		t.Cleanup(func() { _ = store.Close() })

		if err := store.RemoteStore().Set("k", map[string]string{"v": "x"}, 0); err != nil {
			t.Fatal(err)
		}

		var out map[string]string
		if err := store.Get("k", &out); err != nil {
			t.Fatalf("应命中 L2: %v", err)
		}

		remaining := time.Until(time.Unix(0, localExpirationOf(t, store, "k")))
		if remaining <= 25*time.Second || remaining > localTTL {
			t.Fatalf("L1 剩余 TTL = %v，want 接近 %v", remaining, localTTL)
		}
	})
}

// TestTwoLevelStoreLocalExpirationIndependentOfLocalStoreDefault 钉住"永不向 LocalStore 传 0"。
//
// 远端无过期时间时没有剩余 TTL 可以约束回填，此时必须用完整的 localTTL，而不是把 0
// 传下去——传 0 就变成"用 LocalStore 自己的默认值"，L1 寿命于是取决于注入实现的
// 默认值而不是 localTTL。
//
// 写路径改为失效 L1 之后，回填是唯一会写 L1 的路径，因此这里通过读触发回填来验证。
func TestTwoLevelStoreLocalExpirationIndependentOfLocalStoreDefault(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	const localTTL = 30 * time.Second
	// 注入一个默认值为"永不过期"的 L1，模拟自定义 LocalStore：
	// TwoLevelStore 必须自己给出正数 TTL，不能依赖它的默认值。
	local := NewMemoryStore(0)
	store := NewTwoLevelStore(client,
		WithLocalTTL(localTTL),
		WithRemoteTTL(0), // 远端无过期时间
		WithLocalStore(local),
	)
	t.Cleanup(func() { _ = store.Close() })

	// 调用方传 0 表示"交由存储决定"。
	if err := store.Set("k", map[string]string{"v": "old"}, 0); err != nil {
		t.Fatal(err)
	}

	var out map[string]string
	if err := store.Get("k", &out); err != nil {
		t.Fatalf("应命中 L2 并回填: %v", err)
	}

	exp := localExpirationOf(t, store, "k")
	if exp == 0 {
		t.Fatal("L1 expiration = 0（永不过期）——L1 寿命被 LocalStore 的默认值决定了")
	}
	if remaining := time.Until(time.Unix(0, exp)); remaining <= 25*time.Second || remaining > localTTL {
		t.Fatalf("L1 剩余 TTL = %v，want 接近 %v", remaining, localTTL)
	}
}

// TestTwoLevelStoreInvalidationOriginIsNotMemoryAddress 钉住实例身份不再取自内存地址。
//
// 此前用 fmt.Sprintf("%p", s)：对进程内存布局做了无依据假设，地址相同的两个进程
// 会把对方的广播当成自己的而跳过，广播能力静默失效；它还把一个堆地址原样发进
// Pub/Sub 载荷，凡能订阅该 channel 的都读得到。
func TestTwoLevelStoreInvalidationOriginIsNotMemoryAddress(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)

	newStore := func() *TwoLevelStore {
		client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
		t.Cleanup(func() { _ = client.Close() })

		store := NewTwoLevelStore(client, WithTwoLevelInvalidationBroadcast(client, "inv"))
		t.Cleanup(func() { _ = store.Close() })
		return store
	}

	first := newStore()
	second := newStore()

	if first.invalidation == nil || second.invalidation == nil {
		t.Fatal("广播未启动")
	}

	for _, store := range []*TwoLevelStore{first, second} {
		origin := store.invalidation.origin
		if origin == fmt.Sprintf("%p", store) {
			t.Fatalf("origin = %q，仍然是实例内存地址", origin)
		}
		if len(origin) != 32 {
			t.Fatalf("origin = %q，长度 %d，want 32（16 字节 hex）", origin, len(origin))
		}
	}

	if first.invalidation.origin == second.invalidation.origin {
		t.Fatalf("两个实例的 origin 相同：%q", first.invalidation.origin)
	}
}

// TestTwoLevelStoreForgetsInFlightReadOnLocalMutation 钉住本地变更让在飞的读作废。
//
// 远端失效消息一直会 sf.Forget，本地 Set/Delete 此前漏了：本地删除之后新加入的
// 等待者仍会拿到 leader 手里那份变更前读到的值，本地变更的陈旧扩散面反而比
// 远端变更大。
func TestTwoLevelStoreForgetsInFlightReadOnLocalMutation(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := NewTwoLevelStore(client, WithLocalTTL(30*time.Second), WithRemoteTTL(time.Minute))
	t.Cleanup(func() { _ = store.Close() })

	if err := store.RemoteStore().Set("k", map[string]string{"v": "old"}, time.Minute); err != nil {
		t.Fatal(err)
	}

	leaderInFlight := make(chan struct{})
	releaseLeader := make(chan struct{})
	var blocked atomic.Bool

	// 回填走 pipeline（GET+PTTL 合成一个往返），因此要挂在 pipeline 钩子上。
	//
	// 只有首个进来的读被阻塞，后续读必须直接放过：用 sync.Once 会让第二个调用者
	// 等待首个 Do 完成，而放行信号要等它返回才发出——死锁。
	client.AddHook(blockingPipelineHook{block: func() {
		if blocked.CompareAndSwap(false, true) {
			close(leaderInFlight)
			<-releaseLeader
		}
	}})

	var leaderErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		var out map[string]string
		leaderErr = store.Get("k", &out) // leader：阻塞在远端读里
	})

	<-leaderInFlight

	// 变更发生在 leader 那次读之后。
	if err := store.Delete("k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// 新请求不该并进 leader 那条 flight：远端已删，它应当拿到 miss。
	var out map[string]string
	waiterErr := store.Get("k", &out)

	close(releaseLeader)
	wg.Wait()

	if !errors.Is(waiterErr, ErrCacheMiss) {
		t.Fatalf("变更后的 Get err = %v value = %v, want ErrCacheMiss——它并进了变更前发起的那条 flight",
			waiterErr, out)
	}
	t.Logf("leader 自身的结果 err=%v（不受影响）", leaderErr)
}

// TestTwoLevelStoreBackfillDoesNotResurrectMutatedKey 钉住"成功的变更不会被在飞的读撤销"。
//
// sf.Forget 只解除后续请求与旧任务的关联，不取消旧任务、也不阻止它写 L1。调用方
// 拿到 Delete / Set 的成功返回之后，若变更前发起的那次读随后把旧值回填进 L1，
// 本实例就会继续返回它直到 localTTL 到期。代次守卫负责拦下这种回填。
func TestTwoLevelStoreBackfillDoesNotResurrectMutatedKey(t *testing.T) {
	t.Parallel()

	// mutate 在"远端读已完成、回填尚未执行"这个点上被调用，把竞态窗口钉成确定顺序。
	//
	// 四种变更之后 L1 都应当为空：Delete / InvalidateLocal / DeletePattern 本就是删除，
	// 而 Set 的写路径也是失效 L1 而非写入 L1。区别在 Set 之后 Redis 持有新值，
	// 因此额外断言"下一次读拿到新值"。
	tests := []struct {
		name   string
		mutate func(*TwoLevelStore) error
		// wantReadValue 非空时，额外断言变更后的一次读返回该值。
		wantReadValue string
	}{
		{
			name:   "Delete 之后不复活",
			mutate: func(s *TwoLevelStore) error { return s.Delete("k") },
		},
		{
			name: "Set 之后不被旧值覆盖",
			mutate: func(s *TwoLevelStore) error {
				return s.Set("k", map[string]string{"v": "new"}, time.Minute)
			},
			wantReadValue: "new",
		},
		{
			name: "InvalidateLocal 之后不复活",
			mutate: func(s *TwoLevelStore) error {
				s.InvalidateLocal("k")
				return nil
			},
		},
		{
			name: "DeletePattern 之后不复活",
			mutate: func(s *TwoLevelStore) error {
				_, err := s.DeletePattern(context.Background(), "k*")
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mini := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
			t.Cleanup(func() { _ = client.Close() })

			store := NewTwoLevelStore(client, WithLocalTTL(30*time.Second), WithRemoteTTL(time.Minute))
			t.Cleanup(func() { _ = store.Close() })

			if err := store.RemoteStore().Set("k", map[string]string{"v": "old"}, time.Minute); err != nil {
				t.Fatal(err)
			}

			var mutated atomic.Bool
			client.AddHook(blockingPipelineHook{afterExec: func() {
				if mutated.CompareAndSwap(false, true) {
					if err := tc.mutate(store); err != nil {
						t.Errorf("mutate: %v", err)
					}
				}
			}})

			var out map[string]string
			if err := store.Get("k", &out); err != nil {
				t.Fatalf("Get: %v", err)
			}

			var l1 map[string]string
			if err := store.local.Get("k", &l1); err == nil {
				t.Fatalf("变更成功后 L1 仍有残留 %v——旧回填复活了它", l1)
			}

			if tc.wantReadValue == "" {
				return
			}

			// Redis 持有新值，变更后的一次读必须拿到它。
			var got map[string]string
			if err := store.Get("k", &got); err != nil {
				t.Fatalf("变更后的读失败: %v", err)
			}
			if got["v"] != tc.wantReadValue {
				t.Fatalf("变更后读到 %q, want %q", got["v"], tc.wantReadValue)
			}
		})
	}
}

// TestTwoLevelStoreInvalidationMessageFencesInFlightRead 钉住跨实例失效同样能拦下在飞回填。
func TestTwoLevelStoreInvalidationMessageFencesInFlightRead(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := NewTwoLevelStore(client, WithLocalTTL(30*time.Second), WithRemoteTTL(time.Minute))
	t.Cleanup(func() { _ = store.Close() })

	if err := store.RemoteStore().Set("k", map[string]string{"v": "old"}, time.Minute); err != nil {
		t.Fatal(err)
	}

	// 直接调用订阅端的处理动作，等价于收到另一个实例的失效消息，避免依赖
	// Pub/Sub 投递时序。
	var fenced atomic.Bool
	client.AddHook(blockingPipelineHook{afterExec: func() {
		if fenced.CompareAndSwap(false, true) {
			_ = store.guard("k").commitMutation(func() error {
				return store.local.Delete("k")
			})
			store.sf.Forget("k")
		}
	}})

	var out map[string]string
	if err := store.Get("k", &out); err != nil {
		t.Fatalf("Get: %v", err)
	}

	var l1 map[string]string
	if err := store.local.Get("k", &l1); err == nil {
		t.Fatalf("收到失效后 L1 仍有残留 %v——旧回填复活了它", l1)
	}
}

// TestTwoLevelStoreSupersededLeaderDoesNotBackfill 钉住被超时释放的 Get leader 不写 L1。
//
// 它读到的值比新 leader 的更旧，回填上去就是把新的盖成旧的。
func TestTwoLevelStoreSupersededLeaderDoesNotBackfill(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := NewTwoLevelStore(client,
		WithLocalTTL(30*time.Second),
		WithRemoteTTL(time.Minute),
		WithTwoLevelSingleFlightForgetTimeout(50*time.Millisecond),
	)
	t.Cleanup(func() { _ = store.Close() })

	if err := store.RemoteStore().Set("k", map[string]string{"v": "old"}, time.Minute); err != nil {
		t.Fatal(err)
	}

	// 让远端读慢于释放超时，使这条 flight 在读完成前就被释放。
	var slowed atomic.Bool
	client.AddHook(blockingPipelineHook{block: func() {
		if slowed.CompareAndSwap(false, true) {
			time.Sleep(150 * time.Millisecond)
		}
	}})

	var out map[string]string
	if err := store.Get("k", &out); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if out["v"] != "old" {
		t.Fatalf("调用方应当仍拿到自己读到的值，实际 %v", out)
	}

	var l1 map[string]string
	if err := store.local.Get("k", &l1); err == nil {
		t.Fatalf("被释放的 leader 仍然回填了 L1: %v", l1)
	}
}

// TestTwoLevelStoreGuardSharding 钉住分片索引的稳定性与分散性。
func TestTwoLevelStoreGuardSharding(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := NewTwoLevelStore(client)
	t.Cleanup(func() { _ = store.Close() })

	// 同一 key 必须稳定落到同一分片，否则代次校验形同虚设。
	const stableKey = "same"
	first, second := store.guard(stableKey), store.guard(stableKey)
	if first != second {
		t.Fatal("同一 key 落到了不同分片")
	}

	// 代次状态是定长数组，不随 key 数量增长。
	distinct := make(map[*twoLevelKeyGuard]struct{})
	for i := range 4096 {
		distinct[store.guard(fmt.Sprintf("key:%d", i))] = struct{}{}
	}
	if len(distinct) > twoLevelGuardShards {
		t.Fatalf("分片数 = %d，超过上限 %d", len(distinct), twoLevelGuardShards)
	}
	// 4096 个 key 落到 256 个分片，正常情况下应当覆盖绝大多数分片。
	if len(distinct) < twoLevelGuardShards/2 {
		t.Fatalf("4096 个 key 只落到 %d 个分片，分散性不足", len(distinct))
	}
}

// TestPersistNilGuards 钉住构造期对 nil 与 typed-nil 的拒绝。
func TestPersistNilGuards(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	tests := []struct {
		name      string
		construct func()
		wantMsg   string
	}{
		{
			name:      "NewRedisStore 拒绝 nil client",
			construct: func() { NewRedisStore(nil) },
			wantMsg:   "redis client must not be nil",
		},
		{
			name:      "NewRedisStore 拒绝 typed-nil client",
			construct: func() { NewRedisStore((*redis.Client)(nil)) },
			wantMsg:   "redis client must not be nil",
		},
		{
			name:      "NewTwoLevelStore 拒绝 typed-nil client",
			construct: func() { NewTwoLevelStore((*redis.Client)(nil)) },
			wantMsg:   "redisClient must not be nil",
		},
		{
			name:      "WithLocalStore 拒绝 nil",
			construct: func() { NewTwoLevelStore(client, WithLocalStore(nil)) },
			wantMsg:   "local store must not be nil",
		},
		{
			name:      "WithLocalStore 拒绝 typed-nil",
			construct: func() { NewTwoLevelStore(client, WithLocalStore((*MemoryStore)(nil))) },
			wantMsg:   "local store must not be nil",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			defer func() {
				recovered := recover()
				if recovered == nil {
					t.Fatal("没有 panic——失败被推迟到了首次读写")
				}
				msg, _ := recovered.(string)
				if !strings.Contains(msg, tc.wantMsg) {
					t.Fatalf("panic 信息 = %q，want 含 %q", msg, tc.wantMsg)
				}
				if !strings.HasPrefix(msg, "gincache: ") {
					t.Fatalf("panic 信息 = %q，缺少包名前缀", msg)
				}
			}()
			tc.construct()
		})
	}
}

// TestIsNilValue 钉住 typed-nil 判定。
func TestIsNilValue(t *testing.T) {
	t.Parallel()

	var nilClient *redis.Client
	var nilStore *MemoryStore
	var nilFunc func()
	var nilMap map[string]int

	tests := []struct {
		name  string
		value any
		want  bool
	}{
		{"nil 接口", nil, true},
		{"typed-nil 指针", nilClient, true},
		{"typed-nil 实现接口的指针", nilStore, true},
		{"nil 函数", nilFunc, true},
		{"nil map", nilMap, true},
		{"非 nil 指针", &MemoryStore{}, false},
		{"非指针值", 42, false},
		{"空字符串", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := isNilValue(tc.value); got != tc.want {
				t.Fatalf("isNilValue(%#v) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// blockingPipelineHook 在 pipeline 执行前后插入回调，用于把并发窗口钉成确定顺序。
type blockingPipelineHook struct {
	block     func()
	afterExec func()
}

func (blockingPipelineHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

func (blockingPipelineHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		return next(ctx, cmd)
	}
}

func (h blockingPipelineHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if h.block != nil {
			h.block()
		}
		err := next(ctx, cmds)
		if h.afterExec != nil {
			h.afterExec()
		}
		return err
	}
}
