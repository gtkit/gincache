package ristrettoadapter

import (
	"bytes"
	"time"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/gtkit/gincache/persist"
	"github.com/gtkit/gincache/persist/internal/localadapter"
)

type ristrettoEngine struct {
	raw *ristretto.Cache[string, []byte]
}

func validateRistrettoConfig(cfg ristretto.Config[string, []byte]) error {
	if cfg.MaxCost <= 0 || cfg.NumCounters <= 0 || cfg.BufferItems <= 0 || cfg.TtlTickerDurationInSec < 0 ||
		cfg.IgnoreInternalCost || cfg.Cost != nil || cfg.ShouldUpdate != nil || cfg.OnEvict != nil ||
		cfg.OnReject != nil || cfg.OnExit != nil || cfg.KeyToHash != nil {
		return ErrInvalidArgument
	}
	return nil
}

// NewOwner 创建按字节计费的有界实例；不接受改变生命周期/准入的自定义回调。
func NewOwner(cfg ristretto.Config[string, []byte]) (*Owner, error) {
	if err := validateRistrettoConfig(cfg); err != nil {
		return nil, err
	}
	raw, err := ristretto.NewCache(&cfg)
	if err != nil {
		return nil, err
	}
	return localadapter.NewOwner(&ristrettoEngine{raw: raw}, true), nil
}

// WrapOwner 借用实例；cfg 须来自原始构造且符合本包约束，不转移所有权。
func WrapOwner(raw *ristretto.Cache[string, []byte], cfg ristretto.Config[string, []byte]) (*Owner, error) {
	if raw == nil {
		return nil, ErrInvalidArgument
	}
	if err := validateRistrettoConfig(cfg); err != nil {
		return nil, err
	}
	if raw.MaxCost() != cfg.MaxCost {
		return nil, ErrInvalidArgument
	}
	return localadapter.NewOwner(&ristrettoEngine{raw: raw}, false), nil
}

func (e *ristrettoEngine) Get(key string) ([]byte, error) {
	data, ok := e.raw.Get(key)
	if !ok {
		return nil, persist.ErrCacheMiss
	}
	return data, nil
}

func (e *ristrettoEngine) Set(key string, data []byte, ttl time.Duration) (bool, error) {
	if len(key) > int(^uint(0)>>1)-len(data) {
		return false, ErrInvalidArgument
	}
	if !e.raw.SetWithTTL(key, data, int64(len(key)+len(data)), ttl) {
		return false, nil
	}
	e.raw.Wait()
	stored, found := e.raw.Get(key)
	// 完整记录含本次序号，不能以之前仍存在的值冒充本次准入成功。
	return found && bytes.Equal(stored, data), nil
}

func (e *ristrettoEngine) Delete(key string) error {
	e.raw.Del(key)
	e.raw.Wait()
	return nil
}

func (e *ristrettoEngine) Walk(visit func(string, []byte)) error {
	e.raw.IterValues(func(data []byte) bool {
		key, err := localadapter.RecordKey(data)
		if err == nil {
			visit(key, data)
		}
		return false
	})
	return nil
}

func (e *ristrettoEngine) Close() error {
	e.raw.Close()
	return nil
}
