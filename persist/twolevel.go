package persist

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gtkit/json"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// TwoLevelStore 是一个面向生产的两级缓存实现。
// L1 是可插拔的本地缓存，默认使用 MemoryStore。
// L2 是 Redis，并作为共享缓存的事实来源。
// 并发安全性取决于注入的 LocalStore；默认 MemoryStore 和 Redis L2 可并发使用。
type TwoLevelStore struct {
	local     LocalStore
	remote    *RedisStore
	localTTL  time.Duration
	remoteTTL time.Duration
	sf        singleflight.Group
	keyPrefix string

	sfForgetTimeout     time.Duration
	invalidation        *twoLevelInvalidation
	invalidationClient  redis.UniversalClient
	invalidationTimeout time.Duration
	logger              TwoLevelLogger

	localHit  atomic.Uint64
	remoteHit atomic.Uint64
	miss      atomic.Uint64
}

// TwoLevelStoreOption 用于配置 TwoLevelStore。
type TwoLevelStoreOption func(*TwoLevelStore)

// TwoLevelLogger 是 TwoLevelStore 用于记录后台失效广播错误的最小日志接口。
type TwoLevelLogger interface {
	Errorf(format string, args ...any)
}

// WithLocalTTL 设置本地缓存默认 TTL。负数入参被忽略，保持默认值。
func WithLocalTTL(ttl time.Duration) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		if ttl >= 0 {
			s.localTTL = ttl
		}
	}
}

// WithRemoteTTL 设置远端 Redis 默认 TTL。负数入参被忽略，保持默认值。
func WithRemoteTTL(ttl time.Duration) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		if ttl >= 0 {
			s.remoteTTL = ttl
		}
	}
}

// WithTwoLevelKeyPrefix 设置两级缓存使用的 Redis key 前缀。
func WithTwoLevelKeyPrefix(prefix string) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		s.keyPrefix = prefix
	}
}

// WithLocalStore 注入自定义本地缓存实现。
// 自定义实现需要自行保证可被多个 goroutine 并发调用。
func WithLocalStore(local LocalStore) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		s.local = local
	}
}

// WithTwoLevelSingleFlightForgetTimeout 设置 TwoLevelStore 内部 singleflight 的释放时间。
// 超时后会调用 Forget，允许后续同 key 请求重新成为 leader，避免长尾回源阻塞所有等待者。
func WithTwoLevelSingleFlightForgetTimeout(timeout time.Duration) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		s.sfForgetTimeout = timeout
	}
}

// WithTwoLevelLogger 设置 TwoLevelStore 后台路径使用的错误日志接口。
func WithTwoLevelLogger(logger TwoLevelLogger) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		s.logger = logger
	}
}

// WithTwoLevelInvalidationTimeout 设置 L1 失效广播订阅启动确认超时。
func WithTwoLevelInvalidationTimeout(timeout time.Duration) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		if timeout > 0 {
			s.invalidationTimeout = timeout
		}
	}
}

// WithTwoLevelInvalidationBroadcast 开启基于 Redis Pub/Sub 的 L1 失效广播。
// Set/Delete/DeletePattern 成功后会发布失效消息，其他实例收到后清理自己的本地缓存。
func WithTwoLevelInvalidationBroadcast(client redis.UniversalClient, channel string) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		if client == nil || channel == "" {
			return
		}
		s.invalidationClient = client
		s.invalidation = &twoLevelInvalidation{channel: channel}
	}
}

// NewTwoLevelStore 创建一个 L1 可插拔、L2 为 Redis 的两级缓存。
//
// 配置错误在这里就地 panic，而不是拖到第一次读写时才以 nil 解引用的形式暴露：
// redisClient 为 nil 即 panic，opts 中的 nil 元素被跳过。
func NewTwoLevelStore(redisClient redis.Cmdable, opts ...TwoLevelStoreOption) *TwoLevelStore {
	if redisClient == nil {
		panic("gincache: redisClient must not be nil")
	}

	s := &TwoLevelStore{
		localTTL:            30 * time.Second,
		remoteTTL:           5 * time.Minute,
		keyPrefix:           "gincache:2l:",
		sfForgetTimeout:     10 * time.Second,
		invalidationTimeout: 5 * time.Second,
	}

	for _, opt := range opts {
		// 跳过而不是 panic：`var opt TwoLevelStoreOption; if cond { opt = WithX() }`
		// 是常见的条件构造写法。
		if opt != nil {
			opt(s)
		}
	}

	if s.local == nil {
		cleanupInterval := max(s.localTTL/2, 10*time.Second)
		s.local = NewMemoryStore(s.localTTL, WithCleanupInterval(cleanupInterval))
	}

	s.remote = NewRedisStore(redisClient, WithKeyPrefix(s.keyPrefix))
	s.startInvalidation()
	return s
}

// Get 优先读取本地缓存，未命中时回源 Redis，并在远端命中后回填本地。
func (s *TwoLevelStore) Get(key string, value any) error {
	if err := s.local.Get(key, value); err == nil {
		s.localHit.Add(1)
		return nil
	}

	result, err, _ := s.sf.Do(key, func() (any, error) {
		stopForget := s.scheduleSingleFlightForget(key)
		defer stopForget()

		var temp json.RawMessage
		if err := s.local.Get(key, &temp); err == nil {
			return twoLevelResult{data: temp, fromLocal: true}, nil
		}

		var data json.RawMessage
		err := s.remote.Get(key, &data)
		if err != nil {
			return nil, err
		}

		_ = s.local.Set(key, data, s.localTTL)

		return twoLevelResult{data: data}, nil
	})
	if err != nil {
		s.miss.Add(1)
		return err
	}

	got, ok := result.(twoLevelResult)
	if !ok {
		return fmt.Errorf("gincache: unexpected singleflight result type %T", result)
	}

	// 按实际来源计数。等待者与 leader 记为同一来源——他们拿到的确实是同一份
	// 数据、同一个来源；无条件计入远端会把并发回填期间的 L1 命中算成 L2 命中。
	if got.fromLocal {
		s.localHit.Add(1)
	} else {
		s.remoteHit.Add(1)
	}

	return json.Unmarshal(got.data, value)
}

// twoLevelResult 承载一次 singleflight 回源的结果及其命中来源。
type twoLevelResult struct {
	data      json.RawMessage
	fromLocal bool
}

// Set 先写 Redis，成功后再写本地缓存。
func (s *TwoLevelStore) Set(key string, value any, expire time.Duration) error {
	return s.SetWithContext(context.Background(), key, value, expire)
}

// SetWithContext 先写 Redis，再写本地缓存，避免远端失败导致本地脏数据。
func (s *TwoLevelStore) SetWithContext(ctx context.Context, key string, value any, expire time.Duration) error {
	localExpire := s.localTTL
	remoteExpire := expire
	if remoteExpire == 0 {
		remoteExpire = s.remoteTTL
	}

	localExpire = min(localExpire, remoteExpire)

	if err := s.remote.SetWithContext(ctx, key, value, remoteExpire); err != nil {
		return err
	}

	s.publishInvalidation(ctx, twoLevelInvalidationKey, key)

	if err := s.local.Set(key, value, localExpire); err != nil {
		// 关键动作是失效而不只是报错：Redis 已经是新值，L1 里的旧值若原样留着
		// 直到 TTL 到期，本实例这段时间读到的一直是旧的。删掉后读取会穿到 Redis。
		// 删除本身是 best-effort，删也失败时不叠加错误——调用方需要知道的是这次写没成功。
		if delErr := s.local.Delete(key); delErr != nil {
			s.logf("gincache: failed to invalidate stale local entry after local set failure key=%q: %v", key, delErr)
		}
		return fmt.Errorf("gincache: local cache set failed after remote set succeeded key=%q: %w", key, err)
	}

	return nil
}

// Delete 同时删除本地缓存和 Redis 中的单个 key。
// 返回值表达的是 Redis 的删除结果；本地删除失败只记录日志，不混入返回值。
func (s *TwoLevelStore) Delete(key string) error {
	if err := s.local.Delete(key); err != nil {
		s.logf("gincache: failed to delete local key=%q: %v", key, err)
	}

	err := s.remote.Delete(key)
	if err == nil {
		s.publishInvalidation(context.Background(), twoLevelInvalidationKey, key)
	}
	return err
}

// DeletePattern 先尽力删除本地缓存，再删除 Redis 中匹配模式的 key。
// 返回值表达的是 Redis 的删除结果；本地删除失败只记录日志，不混入返回值。
func (s *TwoLevelStore) DeletePattern(ctx context.Context, pattern string) (int64, error) {
	if local, ok := s.local.(LocalStoreWithPattern); ok {
		if _, err := local.DeletePattern(ctx, pattern); err != nil {
			s.logf("gincache: failed to delete local pattern=%q: %v", pattern, err)
		}
	}
	n, err := s.remote.DeletePattern(ctx, pattern)
	if err == nil {
		s.publishInvalidation(ctx, twoLevelInvalidationPattern, pattern)
	}
	return n, err
}

// InvalidateLocal 只删除本地缓存中的单个 key。
func (s *TwoLevelStore) InvalidateLocal(key string) {
	_ = s.local.Delete(key)
}

// InvalidateLocalPattern 在本地缓存支持按模式删除时，清理本地匹配 key。
func (s *TwoLevelStore) InvalidateLocalPattern(pattern string) {
	if local, ok := s.local.(LocalStoreWithPattern); ok {
		_, _ = local.DeletePattern(context.Background(), pattern)
	}
}

// Stats 返回两级缓存的聚合统计信息。
func (s *TwoLevelStore) Stats() map[string]int64 {
	localStats := s.local.Stats()

	localHit := s.localHit.Load()
	remoteHit := s.remoteHit.Load()
	miss := s.miss.Load()
	total := localHit + remoteHit + miss

	var hitRate float64
	if total > 0 {
		hitRate = float64(localHit+remoteHit) / float64(total) * 100
	}

	var localHitRate float64
	if localHit+remoteHit > 0 {
		localHitRate = float64(localHit) / float64(localHit+remoteHit) * 100
	}

	return map[string]int64{
		"local_keys":     localStats["keys"],
		"local_hit":      int64(localHit),
		"remote_hit":     int64(remoteHit),
		"miss":           int64(miss),
		"total_hit_rate": int64(hitRate),
		"local_hit_rate": int64(localHitRate),
	}
}

// ResetStats 重置两级缓存的聚合计数和本地缓存计数。
func (s *TwoLevelStore) ResetStats() {
	s.localHit.Store(0)
	s.remoteHit.Store(0)
	s.miss.Store(0)
	s.local.ResetStats()
}

// Close 关闭本地缓存。
func (s *TwoLevelStore) Close() error {
	s.closeInvalidation()
	return s.local.Close()
}

// LocalStore 返回当前配置的本地缓存实现。
func (s *TwoLevelStore) LocalStore() LocalStore {
	return s.local
}

// RemoteStore 返回 Redis 实现的二级缓存。
func (s *TwoLevelStore) RemoteStore() *RedisStore {
	return s.remote
}

func (s *TwoLevelStore) scheduleSingleFlightForget(key string) func() {
	if s.sfForgetTimeout <= 0 {
		return func() {}
	}

	timer := time.AfterFunc(s.sfForgetTimeout, func() {
		s.sf.Forget(key)
	})
	return func() {
		timer.Stop()
	}
}

const (
	twoLevelInvalidationKey     = "key"
	twoLevelInvalidationPattern = "pattern"

	defaultTwoLevelInvalidationChannelSize = 32
)

type twoLevelInvalidationMessage struct {
	Origin string `json:"origin"`
	Type   string `json:"type"`
	Value  string `json:"value"`
}

type twoLevelInvalidation struct {
	channel string
	origin  string

	pubsub *redis.PubSub
	done   chan struct{}
	once   sync.Once
	closed atomic.Bool
}

func (s *TwoLevelStore) startInvalidation() {
	if s.invalidation == nil || s.invalidation.channel == "" {
		return
	}
	if s.invalidationClient == nil {
		s.invalidation = nil
		return
	}

	s.invalidation.origin = fmt.Sprintf("%p", s)
	s.invalidation.done = make(chan struct{})

	ctx, cancel := context.WithTimeout(context.Background(), s.invalidationTimeout)
	defer cancel()

	pubsub := s.invalidationClient.Subscribe(ctx, s.invalidation.channel)
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		s.logf("gincache: failed to start invalidation subscriber channel=%q: %v", s.invalidation.channel, err)
		s.invalidation = nil
		return
	}

	s.invalidation.pubsub = pubsub

	go s.listenInvalidations()
}

func (s *TwoLevelStore) listenInvalidations() {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logf("gincache: invalidation subscriber panic channel=%q: %v", s.invalidation.channel, recovered)
		}
		close(s.invalidation.done)
	}()

	ch := s.invalidation.pubsub.Channel(redis.WithChannelSize(defaultTwoLevelInvalidationChannelSize))
	for msg := range ch {
		var event twoLevelInvalidationMessage
		if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
			s.logf("gincache: invalid invalidation message channel=%q: %v", s.invalidation.channel, err)
			continue
		}
		if event.Origin == s.invalidation.origin {
			continue
		}

		switch event.Type {
		case twoLevelInvalidationKey:
			if err := s.local.Delete(event.Value); err != nil {
				s.logf("gincache: failed to delete local key from invalidation key=%q: %v", event.Value, err)
			}
			s.sf.Forget(event.Value)
		case twoLevelInvalidationPattern:
			if local, ok := s.local.(LocalStoreWithPattern); ok {
				if _, err := local.DeletePattern(context.Background(), event.Value); err != nil {
					s.logf("gincache: failed to delete local pattern from invalidation pattern=%q: %v", event.Value, err)
				}
			}
		default:
			s.logf("gincache: unknown invalidation message type=%q channel=%q", event.Type, s.invalidation.channel)
		}
	}
	if !s.invalidation.closed.Load() {
		s.logf("gincache: invalidation subscriber channel closed channel=%q", s.invalidation.channel)
	}
}

func (s *TwoLevelStore) publishInvalidation(ctx context.Context, typ, value string) {
	if s.invalidation == nil || s.invalidationClient == nil {
		return
	}

	payload, err := json.Marshal(twoLevelInvalidationMessage{
		Origin: s.invalidation.origin,
		Type:   typ,
		Value:  value,
	})
	if err != nil {
		return
	}

	if err := s.invalidationClient.Publish(ctx, s.invalidation.channel, payload).Err(); err != nil {
		s.logf("gincache: failed to publish invalidation channel=%q type=%q value=%q: %v", s.invalidation.channel, typ, value, err)
	}
}

func (s *TwoLevelStore) closeInvalidation() {
	if s.invalidation == nil || s.invalidation.pubsub == nil {
		return
	}

	s.invalidation.once.Do(func() {
		s.invalidation.closed.Store(true)
		_ = s.invalidation.pubsub.Close()
		<-s.invalidation.done
	})
}

func (s *TwoLevelStore) logf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Errorf(format, args...)
	}
}
