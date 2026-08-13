package persist

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestEscapeGlobMeta 钉住元字符转义。
func TestEscapeGlobMeta(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in, want string
	}{
		{"gincache:", "gincache:"},
		{"", ""},
		{"app*:", `app\*:`},
		{"app?:", `app\?:`},
		{"app[1]:", `app\[1\]:`},
		{`app\:`, `app\\:`},
		{"a*b?c[d]e", `a\*b\?c\[d\]e`},
	}

	for _, tc := range tests {
		if got := escapeGlobMeta(tc.in); got != tc.want {
			t.Errorf("escapeGlobMeta(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRedisStorePrefixWithGlobMetaIsLiteral 钉住带元字符的前缀不改变模式语义。
//
// 前缀被直接拼进 SCAN 模式时，`ns*:` 这样的前缀会匹配到 `nsother:` 等别的命名空间，
// DeletePattern 就把它们一起删了。
func TestRedisStorePrefixWithGlobMetaIsLiteral(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	// 目标命名空间的前缀含元字符；旁边放一个会被通配匹配到的无关命名空间。
	target := NewRedisStore(client, WithKeyPrefix("ns*:"))
	bystander := NewRedisStore(client, WithKeyPrefix("nsother:"))

	if err := target.Set("a", map[string]string{"v": "target"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := bystander.Set("a", map[string]string{"v": "bystander"}, time.Minute); err != nil {
		t.Fatal(err)
	}

	t.Run("Stats 只计本前缀", func(t *testing.T) {
		if got := target.Stats()["keys"]; got != 1 {
			t.Fatalf("Stats keys = %d, want 1——前缀里的通配把别的命名空间也算进来了", got)
		}
	})

	t.Run("单 key 路径仍寻址到字面 key", func(t *testing.T) {
		var out map[string]string
		if err := target.Get("a", &out); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if out["v"] != "target" {
			t.Fatalf("Get 得到 %q, want target", out["v"])
		}
	})

	t.Run("DeletePattern 不误删别的命名空间", func(t *testing.T) {
		n, err := target.DeletePattern(context.Background(), "*")
		if err != nil {
			t.Fatalf("DeletePattern: %v", err)
		}
		if n != 1 {
			t.Fatalf("删除了 %d 个, want 1", n)
		}

		var out map[string]string
		if err := bystander.Get("a", &out); err != nil {
			t.Fatalf("旁观命名空间被误删: %v", err)
		}
		if out["v"] != "bystander" {
			t.Fatalf("旁观命名空间的值变成了 %q", out["v"])
		}
	})
}

// TestRedisStoreCallerWildcardStillWorks 钉住调用方 pattern 的通配语义没被转义掉。
func TestRedisStoreCallerWildcardStillWorks(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := NewRedisStore(client, WithKeyPrefix("ns*:"))
	for _, k := range []string{"user:1", "user:2", "order:1"} {
		if err := store.Set(k, k, time.Minute); err != nil {
			t.Fatal(err)
		}
	}

	n, err := store.DeletePattern(context.Background(), "user:*")
	if err != nil {
		t.Fatalf("DeletePattern: %v", err)
	}
	if n != 2 {
		t.Fatalf("删除了 %d 个, want 2——调用方的通配失效了", n)
	}

	var out string
	if err := store.Get("order:1", &out); err != nil {
		t.Fatalf("不匹配的 key 被删了: %v", err)
	}
}

// TestDeleteEachKeyKeepsPartialCount 钉住部分成功时的删除计数保真。
//
// Pipeline.Exec 返回错误不代表所有命令都没执行；直接返回 0 会让上层分不清
// "部分成功"和"完全失败"——而只有后者不需要广播失效。
func TestDeleteEachKeyKeepsPartialCount(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := NewRedisStore(client, WithKeyPrefix("p:"))
	keys := []string{"p:a", "p:b"}
	for _, k := range keys {
		if err := client.Set(context.Background(), k, "v", time.Minute).Err(); err != nil {
			t.Fatal(err)
		}
	}

	// 让第二条 DEL 报错：命令级失败不应清零已成功的计数。
	var seen atomic.Int64
	client.AddHook(failDelResultHook{seen: &seen})

	deleted, err := deleteEachKey(context.Background(), client, keys)
	t.Logf("deleted=%d err=%v", deleted, err)

	if err == nil {
		t.Fatal("期望返回命令错误")
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1——部分成功的计数被丢弃了", deleted)
	}
	_ = store
}

// TestTwoLevelStorePartialPatternDeleteStillBroadcasts 钉住部分成功也广播失效。
//
// 扇出后"部分分片成功、另一些失败"是可达组合。已删掉的 key 若不广播，其他实例的 L1
// 会继续供旧数据到 localTTL；多失效一次只让对方多读一次 Redis，是安全方向。
func TestTwoLevelStorePartialPatternDeleteStillBroadcasts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		failAll       bool
		wantBroadcast bool
	}{
		{name: "部分成功仍广播", failAll: false, wantBroadcast: true},
		{name: "一个都没删掉不广播", failAll: true, wantBroadcast: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mini := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
			t.Cleanup(func() { _ = client.Close() })

			store := NewTwoLevelStore(client,
				WithLocalTTL(time.Minute),
				WithTwoLevelLogger(&captureLogger{}),
				WithTwoLevelInvalidationBroadcast(client, "gincache:test:partial"),
			)
			t.Cleanup(func() { _ = store.Close() })

			for i := range 2 {
				if err := store.RemoteStore().Set(fmt.Sprintf("k%d", i), i, time.Minute); err != nil {
					t.Fatal(err)
				}
			}

			var dels, published atomic.Int64
			client.AddHook(failDelResultHook{seen: &dels, failAll: tc.failAll})
			client.AddHook(countPublishHook{n: &published})

			published.Store(0)
			n, err := store.DeletePattern(context.Background(), "k*")
			t.Logf("n=%d err=%v published=%d", n, err, published.Load())

			if err == nil {
				t.Fatal("期望返回删除错误")
			}
			if tc.wantBroadcast && n == 0 {
				t.Fatalf("用例前提不成立：期望部分成功但 n=0")
			}
			if !tc.wantBroadcast && n != 0 {
				t.Fatalf("用例前提不成立：期望全失败但 n=%d", n)
			}
			if got := published.Load() > 0; got != tc.wantBroadcast {
				t.Fatalf("广播发出 = %v, want %v（n=%d）", got, tc.wantBroadcast, n)
			}
		})
	}
}

// failDelResultHook 把 pipeline 中 DEL 的结果改成错误：failAll 为真时全部改，
// 否则只改除第一条以外的，用来构造"部分成功"。
type failDelResultHook struct {
	seen    *atomic.Int64
	failAll bool
}

func (failDelResultHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (failDelResultHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }

func (h failDelResultHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		for _, cmd := range cmds {
			if cmd.Name() != "del" {
				continue
			}
			i := h.seen.Add(1)
			if h.failAll || i > 1 {
				cmd.SetErr(errors.New("injected del failure"))
			}
		}
		return err
	}
}

// countPublishHook 统计 PUBLISH 次数。
type countPublishHook struct{ n *atomic.Int64 }

func (countPublishHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h countPublishHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "publish" {
			h.n.Add(1)
		}
		return next(ctx, cmd)
	}
}

func (countPublishHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// TestTwoLevelStoreSetIsBounded 钉住无 Context 的 Set 自带上界。
//
// 它此前用 context.Background()，而 RedisStore 的写超时只在 RedisStore.Set 里生效，
// 因此这条路径完全没有上界。
func TestTwoLevelStoreSetIsBounded(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := NewTwoLevelStore(client, WithLocalTTL(time.Minute))
	t.Cleanup(func() { _ = store.Close() })

	// 把远端写超时压到很短，再让 SET 阻塞得更久。
	store.RemoteStore().writeTimeout = 50 * time.Millisecond
	client.AddHook(blockCommandHook{name: "set", delay: 400 * time.Millisecond})

	start := time.Now()
	err := store.Set("k", "v", time.Minute)
	elapsed := time.Since(start)

	t.Logf("Set 耗时 %v，err=%v", elapsed.Round(time.Millisecond), err)
	if err == nil {
		t.Fatal("期望因超时返回错误")
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("Set 耗时 %v，远超写超时——这条路径没有上界", elapsed)
	}
}

// blockCommandHook 让指定命令阻塞一段时间。
type blockCommandHook struct {
	name  string
	delay time.Duration
}

func (blockCommandHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h blockCommandHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == h.name {
			select {
			case <-time.After(h.delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return next(ctx, cmd)
	}
}

func (blockCommandHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// TestNewTwoLevelStoreRequiresLoggerWithBroadcast 钉住广播必须配 logger。
func TestNewTwoLevelStoreRequiresLoggerWithBroadcast(t *testing.T) {
	t.Parallel()

	t.Run("开广播不配 logger 构造期 panic", func(t *testing.T) {
		t.Parallel()

		mini := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
		t.Cleanup(func() { _ = client.Close() })

		defer func() {
			recovered := recover()
			if recovered == nil {
				t.Fatal("没有 panic——广播启动失败将无从得知")
			}
			msg, _ := recovered.(string)
			if !strings.Contains(msg, "WithTwoLevelLogger") {
				t.Fatalf("panic 信息 = %q，未指出需要配 logger", msg)
			}
		}()
		NewTwoLevelStore(client, WithTwoLevelInvalidationBroadcast(client, "gincache:test:nologger"))
	})

	t.Run("不开广播不要求 logger", func(t *testing.T) {
		t.Parallel()

		mini := miniredis.RunT(t)
		client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
		t.Cleanup(func() { _ = client.Close() })

		store := NewTwoLevelStore(client)
		t.Cleanup(func() { _ = store.Close() })
	})
}

// TestTwoLevelStoreCloseIsIdempotentForInjectedStore 钉住 Close 的幂等承诺。
//
// LocalStore 契约没有要求 Close 幂等，而 WithLocalStore 接受任意实现；
// 此前每次 Close 都会再调一次注入实现的 Close，"重复调用是安全的"只对默认
// MemoryStore 成立。
func TestTwoLevelStoreCloseIsIdempotentForInjectedStore(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	local := &countingCloseLocalStore{err: errors.New("close failed")}
	store := NewTwoLevelStore(client, WithLocalTTL(time.Minute), WithLocalStore(local))

	first := store.Close()
	second := store.Close()
	third := store.Close()

	if got := local.closes.Load(); got != 1 {
		t.Fatalf("注入实现的 Close 被调用 %d 次, want 1", got)
	}
	if !errors.Is(first, local.err) {
		t.Fatalf("首次 Close 返回 %v, want %v", first, local.err)
	}
	if !errors.Is(second, local.err) || !errors.Is(third, local.err) {
		t.Fatalf("后续 Close 返回 %v / %v，与首次结果不一致", second, third)
	}
}

// countingCloseLocalStore 记录 Close 被调用的次数。
type countingCloseLocalStore struct {
	closes atomic.Int64
	err    error
}

func (c *countingCloseLocalStore) Get(string, any) error                { return ErrCacheMiss }
func (c *countingCloseLocalStore) Set(string, any, time.Duration) error { return nil }
func (c *countingCloseLocalStore) Delete(string) error                  { return nil }
func (c *countingCloseLocalStore) Stats() map[string]int64              { return map[string]int64{"keys": 0} }
func (c *countingCloseLocalStore) ResetStats()                          {}

func (c *countingCloseLocalStore) Close() error {
	c.closes.Add(1)
	return c.err
}
