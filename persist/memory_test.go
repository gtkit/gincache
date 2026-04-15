package persist

import (
	"testing"
	"time"
)

func TestNewMemoryStoreFallsBackToDefaultCleanupIntervalWhenConfiguredNonPositive(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore(time.Minute, WithCleanupInterval(0))
	t.Cleanup(func() {
		_ = store.Close()
	})

	if got := store.cleanupInterval; got != time.Minute {
		t.Fatalf("cleanupInterval = %v, want %v", got, time.Minute)
	}
}
