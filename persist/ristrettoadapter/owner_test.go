package ristrettoadapter

import (
	"errors"
	"github.com/gtkit/gincache/persist/internal/adaptertest"
	"testing"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/gtkit/gincache/persist"
)

func ristrettoConfig() ristretto.Config[string, []byte] {
	return ristretto.Config[string, []byte]{MaxCost: 16 << 20, NumCounters: 10000, BufferItems: 64}
}

func TestRistrettoContract(t *testing.T) {
	adaptertest.Run(t, func(t *testing.T) *Owner {
		owner, err := NewOwner(ristrettoConfig())
		if err != nil {
			t.Fatal(err)
		}
		adaptertest.CloseOwner(t, owner)
		return owner
	})
}

func TestRistrettoRejectAndDeleteBarrier(t *testing.T) {
	cfg := ristrettoConfig()
	cfg.MaxCost = 1
	owner, err := NewOwner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.CloseOwner(t, owner)
	store := adaptertest.NewStore(t, owner, "reject")
	if stored, err := store.SetBytes("key", []byte("value"), 0); stored || err != nil {
		t.Fatalf("policy refusal: %t %v", stored, err)
	}

	owner, err = NewOwner(ristrettoConfig())
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.CloseOwner(t, owner)
	store = adaptertest.NewStore(t, owner, "sequence")
	for range 100 {
		if err := store.Set("key", "value", 0); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete("key"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.GetBytes("key"); !errors.Is(err, persist.ErrCacheMiss) {
			t.Fatalf("revived: %v", err)
		}
	}
}

func TestRistrettoBorrowAndConfig(t *testing.T) {
	cfg := ristrettoConfig()
	raw, err := ristretto.NewCache(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(raw.Close)
	owner, err := WrapOwner(raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !raw.Set("external", []byte("v"), 1) {
		t.Fatal("borrowed engine closed")
	}
	raw.Wait()
	if _, ok := raw.Get("external"); !ok {
		t.Fatal("borrowed engine unavailable")
	}
	cfg.IgnoreInternalCost = true
	if _, err := NewOwner(cfg); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("cost: %v", err)
	}
	cfg = ristrettoConfig()
	cfg.OnExit = func([]byte) {}
	if _, err := NewOwner(cfg); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("callback: %v", err)
	}
}

func TestGincacheAndL1Integration(t *testing.T) {
	owner, err := NewOwner(ristrettoConfig())
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.CloseOwner(t, owner)
	adaptertest.RunIntegration(t, owner)
}
