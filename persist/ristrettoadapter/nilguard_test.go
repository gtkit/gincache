package ristrettoadapter

import (
	"strings"
	"testing"
	"time"

	"github.com/dgraph-io/ristretto/v2"
)

func newTestCache(t *testing.T) *ristretto.Cache[string, []byte] {
	t.Helper()

	cache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: 1000,
		MaxCost:     1 << 20,
		BufferItems: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.Close)
	return cache
}

// TestNilGuards 钉住构造期对 nil 输入的拒绝。
//
// New(nil) 此前不会 panic：Ristretto 的方法对 nil receiver 是安全的，于是它静默
// 变成黑洞缓存——每次 Set 返回 nil 装作写成功，每次 Get 返回未命中。延迟 panic
// 至少会被发现，静默失效不会。
func TestNilGuards(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		construct func()
		wantMsg   string
	}{
		{
			name:      "New 拒绝 nil cache",
			construct: func() { New(nil) },
			wantMsg:   "cache must not be nil",
		},
		{
			name:      "WithLocalStore 拒绝 nil cache",
			construct: func() { WithLocalStore(nil) },
			wantMsg:   "cache must not be nil",
		},
		{
			name:      "WithCost 拒绝 nil 函数",
			construct: func() { New(newTestCache(t), WithCost(nil)) },
			wantMsg:   "cost function must not be nil",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				recovered := recover()
				if recovered == nil {
					t.Fatal("没有 panic——失败被推迟或被静默吞掉")
				}
				msg, _ := recovered.(string)
				if !strings.Contains(msg, tc.wantMsg) {
					t.Fatalf("panic 信息 = %q，want 含 %q", msg, tc.wantMsg)
				}
				if !strings.HasPrefix(msg, "ristrettoadapter: ") {
					t.Fatalf("panic 信息 = %q，缺少包名前缀", msg)
				}
			}()
			tc.construct()
		})
	}
}

// TestValidInputsStillConstruct 钉住正常输入仍能构造并工作。
func TestValidInputsStillConstruct(t *testing.T) {
	t.Parallel()

	store := New(newTestCache(t), WithWait(),
		WithCost(func(_ string, value []byte) int64 { return int64(len(value)) }),
	)
	t.Cleanup(func() { _ = store.Close() })

	if err := store.Set("k", map[string]string{"v": "1"}, time.Minute); err != nil {
		t.Fatal(err)
	}

	var out map[string]string
	if err := store.Get("k", &out); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if out["v"] != "1" {
		t.Fatalf("value = %v, want v=1", out)
	}
}
