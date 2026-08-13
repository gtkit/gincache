package persist

import (
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// BenchmarkRedisStoreRead 对比 L2 读取的两种形状：只取值（GET），以及同时取值与
// 剩余 TTL（GET+PTTL 合成一个 pipeline）。
//
// 回填 TTL 必须受远端剩余 TTL 约束，因此读取要多带回一个剩余时间。往返次数不变，
// 这里量的是多出来的那条命令在服务端处理与客户端解析上的开销——miniredis 在进程
// 内，测不到网络往返，而往返次数本就没变，正好把差异孤立在这部分开销上。
func BenchmarkRedisStoreRead(b *testing.B) {
	mini := miniredis.RunT(b)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	b.Cleanup(func() { _ = client.Close() })

	store := NewRedisStore(client, WithKeyPrefix("bench:"))
	payload := map[string]string{"name": "value", "other": "field"}
	if err := store.Set("k", payload, time.Hour); err != nil {
		b.Fatal(err)
	}

	b.Run("Get", func(b *testing.B) {
		b.ReportAllocs()
		var out map[string]string
		for b.Loop() {
			if err := store.Get("k", &out); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("GetWithTTL", func(b *testing.B) {
		b.ReportAllocs()
		var out map[string]string
		for b.Loop() {
			if _, err := store.getWithTTL("k", &out); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkTwoLevelStoreGet 覆盖两级缓存的两条读取路径：L1 命中，以及 L1 未命中后
// 回源 L2 并回填。后者是本次回填改动所在的热路径。
func BenchmarkTwoLevelStoreGet(b *testing.B) {
	mini := miniredis.RunT(b)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	b.Cleanup(func() { _ = client.Close() })

	store := NewTwoLevelStore(client, WithLocalTTL(time.Minute), WithRemoteTTL(time.Hour))
	b.Cleanup(func() { _ = store.Close() })

	payload := map[string]string{"name": "value", "other": "field"}
	if err := store.Set("k", payload, time.Hour); err != nil {
		b.Fatal(err)
	}

	b.Run("L1Hit", func(b *testing.B) {
		b.ReportAllocs()
		var out map[string]string
		for b.Loop() {
			if err := store.Get("k", &out); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("L2HitAndBackfill", func(b *testing.B) {
		b.ReportAllocs()
		var out map[string]string
		for b.Loop() {
			// 每轮先清掉 L1，强制走 L2 回源加回填。
			store.InvalidateLocal("k")
			if err := store.Get("k", &out); err != nil {
				b.Fatal(err)
			}
		}
	})
}
