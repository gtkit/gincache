package freecacheadapter

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coocood/freecache"
	"github.com/gtkit/gincache/persist/internal/adaptertest"
)

func TestFreeCacheContract(t *testing.T) {
	adaptertest.Run(t, func(t *testing.T) *Owner {
		owner, err := NewOwner(16 << 20)
		if err != nil {
			t.Fatal(err)
		}
		adaptertest.CloseOwner(t, owner)
		return owner
	})
}

func TestFreeCacheRefusalAndBorrowedOwner(t *testing.T) {
	raw := freecache.NewCache(minFreeCacheSize)
	owner, err := WrapOwner(raw, minFreeCacheSize)
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.CloseOwner(t, owner)
	store := adaptertest.NewStore(t, owner, "small")
	if stored, err := store.SetBytes("huge", make([]byte, 1024), 0); err != nil || stored {
		t.Fatalf("refusal: %t %v", stored, err)
	}
	if stored, err := store.SetBytes(strings.Repeat("x", 65536), nil, 0); stored || !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("large key: %t %v", stored, err)
	}
	if err := owner.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := raw.Set([]byte("still-open"), []byte("v"), 0); err != nil {
		t.Fatal(err)
	}
	if got, err := raw.Get([]byte("still-open")); err != nil || string(got) != "v" {
		t.Fatalf("borrowed: %q %v", got, err)
	}
}

func TestFreeCacheValidationAndExpiration(t *testing.T) {
	if _, err := NewOwner(minFreeCacheSize - 1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal(err)
	}
	if _, err := WrapOwner(nil, minFreeCacheSize); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal(err)
	}
	for _, test := range []struct {
		ttl  time.Duration
		want int
	}{{0, 0}, {time.Nanosecond, 2}, {time.Second, 2}, {time.Second + 1, 3}, {time.Duration(1<<63 - 1), 0}} {
		if got := freeExpiration(test.ttl); got != test.want {
			t.Fatalf("ttl %v: %d, want %d", test.ttl, got, test.want)
		}
	}
}

func TestGincacheAndL1Integration(t *testing.T) {
	owner, err := NewOwner(16 << 20)
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.CloseOwner(t, owner)
	adaptertest.RunIntegration(t, owner)
}
