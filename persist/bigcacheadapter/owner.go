package bigcacheadapter

import (
	"context"
	"errors"
	"time"

	"github.com/allegro/bigcache/v3"
	"github.com/gtkit/gincache/persist"
	"github.com/gtkit/gincache/persist/internal/localadapter"
)

type bigEngine struct{ raw *bigcache.BigCache }

func validateBigConfig(cfg bigcache.Config) error {
	if cfg.Shards <= 0 || cfg.Shards&(cfg.Shards-1) != 0 || cfg.HardMaxCacheSize <= 0 ||
		cfg.HardMaxCacheSize > int(^uint(0)>>1)/(1<<20) || cfg.MaxEntrySize < 0 || cfg.MaxEntriesInWindow < 0 ||
		cfg.LifeWindow < 0 || (cfg.CleanWindow > 0 && cfg.LifeWindow < time.Second) {
		return ErrInvalidArgument
	}
	return nil
}

// NewOwner 创建有界 BigCache。ctx 是原生清扫器的生命周期上下文，容量单位为 MB。
func NewOwner(ctx context.Context, cfg bigcache.Config) (*Owner, error) {
	if ctx == nil {
		return nil, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateBigConfig(cfg); err != nil {
		return nil, err
	}
	raw, err := bigcache.New(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return localadapter.NewOwner(&bigEngine{raw: raw}, true), nil
}

// WrapOwner 借用已有实例；cfg 必须与原始构造配置一致，包装不关闭裸实例。
func WrapOwner(raw *bigcache.BigCache, cfg bigcache.Config) (*Owner, error) {
	if raw == nil {
		return nil, ErrInvalidArgument
	}
	if err := validateBigConfig(cfg); err != nil {
		return nil, err
	}
	return localadapter.NewOwner(&bigEngine{raw: raw}, false), nil
}

func (e *bigEngine) Get(key string) ([]byte, error) {
	data, err := e.raw.Get(key)
	if errors.Is(err, bigcache.ErrEntryNotFound) {
		return nil, persist.ErrCacheMiss
	}
	return data, err
}

func (e *bigEngine) Set(key string, data []byte, _ time.Duration) (bool, error) {
	err := e.raw.Set(key, data)
	return err == nil, err
}

func (e *bigEngine) Delete(key string) error {
	err := e.raw.Delete(key)
	if errors.Is(err, bigcache.ErrEntryNotFound) {
		return nil
	}
	return err
}

func (e *bigEngine) Walk(visit func(string, []byte)) error {
	iterator := e.raw.Iterator()
	for iterator.SetNext() {
		entry, err := iterator.Value()
		if err != nil {
			return err
		}
		visit(entry.Key(), entry.Value())
	}
	return nil
}

func (e *bigEngine) Close() error { return e.raw.Close() }
