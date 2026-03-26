package persist

import (
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestTwoLevelStoreUsesInjectedLocalStore(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
	})

	local := NewMemoryStore(time.Minute)
	t.Cleanup(func() {
		_ = local.Close()
	})

	store := NewTwoLevelStore(client, WithLocalStore(local))
	t.Cleanup(func() {
		_ = store.Close()
	})

	if store.LocalStore() != local {
		t.Fatal("TwoLevelStore did not keep injected LocalStore")
	}
}
