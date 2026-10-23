package freecacheadapter

import (
	"errors"
	"time"

	"github.com/coocood/freecache"
	"github.com/gtkit/gincache/persist"
	"github.com/gtkit/gincache/persist/internal/localadapter"
)

const minFreeCacheSize = 512 << 10

type freeEngine struct{ raw *freecache.Cache }

// NewOwner 创建有界实例，容量单位为字节，至少为 512 KiB。
func NewOwner(capacityBytes int) (*Owner, error) {
	if capacityBytes < minFreeCacheSize {
		return nil, ErrInvalidArgument
	}
	return localadapter.NewOwner(&freeEngine{raw: freecache.NewCache(capacityBytes)}, true), nil
}

// WrapOwner 借用已有实例；capacityBytes 必须来自原始创建参数，不转移所有权。
func WrapOwner(raw *freecache.Cache, capacityBytes int) (*Owner, error) {
	if raw == nil || capacityBytes < minFreeCacheSize {
		return nil, ErrInvalidArgument
	}
	return localadapter.NewOwner(&freeEngine{raw: raw}, false), nil
}

func (e *freeEngine) Get(key string) ([]byte, error) {
	data, err := e.raw.Get([]byte(key))
	if errors.Is(err, freecache.ErrNotFound) {
		return nil, persist.ErrCacheMiss
	}
	return data, err
}

func freeExpiration(ttl time.Duration) int {
	if ttl <= 0 {
		return 0
	}
	seconds := int64(ttl/time.Second) + 1
	if ttl%time.Second != 0 {
		seconds++
	}
	now := time.Now().Unix()
	if now < 0 || now > int64(^uint32(0))-seconds || seconds > int64(int(^uint(0)>>1)) {
		return 0
	}
	return int(seconds)
}

func (e *freeEngine) Set(key string, data []byte, ttl time.Duration) (bool, error) {
	err := e.raw.Set([]byte(key), data, freeExpiration(ttl))
	if errors.Is(err, freecache.ErrLargeEntry) {
		return false, nil
	}
	if errors.Is(err, freecache.ErrLargeKey) {
		return false, errors.Join(ErrInvalidArgument, err)
	}
	return err == nil, err
}

func (e *freeEngine) Delete(key string) error {
	e.raw.Del([]byte(key))
	return nil
}

func (e *freeEngine) Walk(visit func(string, []byte)) error {
	iterator := e.raw.NewIterator()
	for item := iterator.Next(); item != nil; item = iterator.Next() {
		visit(string(item.Key), item.Value)
	}
	return nil
}

func (e *freeEngine) Close() error {
	e.raw = nil
	return nil
}
