package localadapter

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gtkit/gincache/persist"
	"github.com/gtkit/json"
)

type storeOptions struct {
	ttl time.Duration
	now func() time.Time
}

// Option 配置独立视图，校验失败由 NewStore 返回。
type Option func(*storeOptions) error

// WithDefaultExpiration 设置零 TTL 的回落值；默认零表示无逻辑期限。
func WithDefaultExpiration(ttl time.Duration) Option {
	return func(o *storeOptions) error {
		if ttl < 0 {
			return ErrInvalidArgument
		}
		o.ttl = ttl
		return nil
	}
}

// WithClock 提供逻辑时钟；与业务 Cache/ByteBackend 配套使用时必须保持一致。
func WithClock(now func() time.Time) Option {
	return func(o *storeOptions) error {
		if now == nil {
			return ErrInvalidArgument
		}
		o.now = now
		return nil
	}
}

// Store 是 Owner 的命名空间视图。Close 仅关闭本视图，不释放原生实例。
type Store struct {
	owner                                          *Owner
	prefix                                         string
	options                                        storeOptions
	closed                                         atomic.Bool
	hits, misses, sets, deletes, rejects, failures atomic.Uint64
}

var (
	_ persist.CacheStore = (*Store)(nil)
	_ persist.LocalStore = (*Store)(nil)
)

// NewStore 绑定固定命名空间。同一 Owner 不允许重复绑定，关闭后也不复用名称。
func (o *Owner) NewStore(namespace string, opts ...Option) (*Store, error) {
	if !validNamespace(namespace) {
		return nil, ErrInvalidArgument
	}
	options := storeOptions{now: time.Now}
	for _, opt := range opts {
		if opt != nil {
			if err := opt(&options); err != nil {
				return nil, err
			}
		}
	}
	if err := o.acquire(context.Background(), false); err != nil {
		return nil, err
	}
	defer o.release()
	if _, exists := o.stores[namespace]; exists {
		return nil, ErrInvalidArgument
	}
	s := &Store{owner: o, prefix: "gac2:" + strconv.Itoa(len(namespace)) + ":" + namespace + ":", options: options}
	o.stores[namespace] = s
	return s, nil
}

func (s *Store) acquire(ctx context.Context) error {
	if s == nil || s.owner == nil {
		return ErrInvalidArgument
	}
	if s.closed.Load() {
		return ErrClosed
	}
	if err := s.owner.acquire(ctx, false); err != nil {
		return err
	}
	if s.closed.Load() {
		s.owner.release()
		return ErrClosed
	}
	return nil
}

// GetBytes 返回独立的字节副本；不存在或达到逻辑期限返回 persist.ErrCacheMiss。
func (s *Store) GetBytes(key string) ([]byte, error) {
	if err := s.acquire(context.Background()); err != nil {
		return nil, err
	}
	defer s.owner.release()
	physical := s.prefix + key
	data, err := s.owner.engine.Get(physical)
	if err != nil {
		if errors.Is(err, persist.ErrCacheMiss) {
			s.misses.Add(1)
		} else {
			s.failures.Add(1)
		}
		return nil, err
	}
	r, err := decodeRecord(data)
	if err != nil {
		s.failures.Add(1)
		return nil, err
	}
	if r.key != physical || (r.expires && !s.options.now().Before(r.expiresAt)) {
		s.misses.Add(1)
		return nil, persist.ErrCacheMiss
	}
	s.hits.Add(1)
	return slices.Clone(r.payload), nil
}

// SetBytes 尝试存储；容量/准入拒绝返回 false、nil，不保证未来命中。
func (s *Store) SetBytes(key string, value []byte, ttl time.Duration) (bool, error) {
	if s == nil || s.owner == nil {
		return false, ErrInvalidArgument
	}
	return s.setBytes(context.Background(), key, value, ttl, s.options.now())
}

func (s *Store) setBytes(ctx context.Context, key string, value []byte, ttl time.Duration, started time.Time) (bool, error) {
	if s == nil || s.owner == nil || ctx == nil || ttl < 0 {
		return false, ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if ttl == 0 {
		ttl = s.options.ttl
	}
	var deadline time.Time
	if ttl > 0 {
		deadline = started.Add(ttl)
	}
	physical := s.prefix + key
	data, err := encodeRecord(physical, value, deadline, ttl > 0)
	if err != nil {
		return false, err
	}
	if err := s.acquire(ctx); err != nil {
		return false, err
	}
	defer s.owner.release()
	if ttl > 0 {
		ttl = deadline.Sub(s.options.now())
		if ttl <= 0 {
			s.rejects.Add(1)
			return false, nil
		}
	}
	if s.owner.sequence == ^uint64(0) {
		return false, ErrInvalidArgument
	}
	s.owner.sequence++
	binary.BigEndian.PutUint64(data[17:25], s.owner.sequence)
	stored, err := s.owner.engine.Set(physical, data, ttl)
	s.sets.Add(1)
	if err != nil {
		s.failures.Add(1)
		return false, err
	}
	if !stored {
		s.rejects.Add(1)
	}
	// 原生同步调用不可被中途打断；取消错误不代表已发生的写入被回滚。
	return stored, ctx.Err()
}

// Get 读取并解码 JSON 值。
func (s *Store) Get(key string, value any) error {
	data, err := s.GetBytes(key)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		s.failures.Add(1)
		return err
	}
	return nil
}

// Set 编码并尝试缓存；正常准入拒绝仍返回 nil。
func (s *Store) Set(key string, value any, ttl time.Duration) error {
	return s.SetWithContext(context.Background(), key, value, ttl)
}

// SetWithContext 在编码前、排队和原生操作前后检查 ctx。已进入的原生调用不可中断。
func (s *Store) SetWithContext(ctx context.Context, key string, value any, ttl time.Duration) error {
	if ctx == nil || s == nil || s.owner == nil || ttl < 0 {
		return ErrInvalidArgument
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	started := s.options.now()
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.setBytes(ctx, key, data, ttl, started)
	return err
}

// Delete 幂等删除本视图的单键。完整键检查与原生删除在同一操作门内。
func (s *Store) Delete(key string) error {
	if err := s.acquire(context.Background()); err != nil {
		return err
	}
	defer s.owner.release()
	physical := s.prefix + key
	data, err := s.owner.engine.Get(physical)
	if errors.Is(err, persist.ErrCacheMiss) {
		return nil
	}
	if err != nil {
		s.failures.Add(1)
		return err
	}
	r, err := decodeRecord(data)
	if err != nil {
		s.failures.Add(1)
		return err
	}
	if r.key != physical {
		return nil
	}
	if err := s.owner.engine.Delete(physical); err != nil {
		s.failures.Add(1)
		return err
	}
	s.deletes.Add(1)
	return nil
}

// Close 仅关闭本视图，等待已进入的原生操作完成。
func (s *Store) Close() error {
	if s == nil || s.owner == nil {
		return nil
	}
	if err := s.owner.acquire(context.Background(), true); err != nil {
		return err
	}
	defer s.owner.release()
	s.closed.Store(true)
	return nil
}

func count(value uint64) int64 { return int64(min(value, uint64(1<<63-1))) }

// Stats 返回本视图计数及 O(N) 原生枚举快照；keys 不是内存配额或有效命中保证。
func (s *Store) Stats() map[string]int64 {
	if s == nil || s.owner == nil {
		return map[string]int64{"closed": 1}
	}
	stats := map[string]int64{"hit": count(s.hits.Load()), "miss": count(s.misses.Load()),
		"set": count(s.sets.Load()), "del": count(s.deletes.Load()),
		"reject": count(s.rejects.Load()), "error": count(s.failures.Load())}
	if err := s.acquire(context.Background()); err != nil {
		stats["closed"] = 1
		return stats
	}
	defer s.owner.release()
	var keys, size, expired int64
	err := s.owner.engine.Walk(func(key string, data []byte) {
		if !strings.HasPrefix(key, s.prefix) {
			return
		}
		r, err := decodeRecord(data)
		if err != nil || r.key != key {
			return
		}
		keys++
		size += int64(len(key)) + int64(len(data))
		if r.expires && !s.options.now().Before(r.expiresAt) {
			expired++
		}
	})
	if err != nil {
		s.failures.Add(1)
		stats["scan_error"] = 1
		return stats
	}
	stats["keys"], stats["bytes"], stats["expired"] = keys, size, expired
	return stats
}

// ResetStats 只清本视图的操作计数，保留数据、序号和其它视图统计。
func (s *Store) ResetStats() {
	s.hits.Store(0)
	s.misses.Store(0)
	s.sets.Store(0)
	s.deletes.Store(0)
	s.rejects.Store(0)
	s.failures.Store(0)
}
