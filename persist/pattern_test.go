package persist

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryStoreDeletePatternMatchesWildcardInMiddle(t *testing.T) {
	t.Parallel()

	store := NewMemoryStore(time.Minute)
	t.Cleanup(func() {
		_ = store.Close()
	})

	for key, value := range map[string]map[string]any{
		"user:1:profile":  {"name": "alice"},
		"user:2:profile":  {"name": "bob"},
		"user:1:settings": {"theme": "dark"},
	} {
		if err := store.Set(key, value, time.Minute); err != nil {
			t.Fatalf("Set(%s) error: %v", key, err)
		}
	}

	if _, err := store.DeletePattern(context.Background(), "user:*:profile"); err != nil {
		t.Fatalf("DeletePattern error: %v", err)
	}

	var got map[string]any
	if err := store.Get("user:1:profile", &got); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("Get user:1:profile err = %v, want %v", err, ErrCacheMiss)
	}
	if err := store.Get("user:2:profile", &got); !errors.Is(err, ErrCacheMiss) {
		t.Fatalf("Get user:2:profile err = %v, want %v", err, ErrCacheMiss)
	}
	if err := store.Get("user:1:settings", &got); err != nil {
		t.Fatalf("Get user:1:settings error: %v", err)
	}
}
