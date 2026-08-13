package persist

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"hash/maphash"
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
//
// 使用约束：
//
// L1 读到的数据最多陈旧 localTTL（见 WithLocalTTL）。这个上限由两条规则维持：
// 写入时 L1 的 TTL 取 min(localTTL, 远端 TTL)，L2 命中回填时取
// min(localTTL, 远端剩余 TTL)，因此 L1 条目不会活过对应的 L2 条目。需要更小的
// 陈旧窗口就调小 localTTL；需要在变更后立刻收敛，用
// WithTwoLevelInvalidationBroadcast 让其他实例即时清理 L1。
//
// 写路径失效 L1 而不写入 L1：Set 成功后本实例该 key 的下一次读会穿到 Redis 并回填。
// 写入 L1 会让两级的最终值可能相反——远端写在守卫的临界区之外，两个并发 Set 的远端
// 写顺序与 L1 写顺序可以颠倒。
//
// 单个 key 的变更（Set / Delete / InvalidateLocal）会让该 key 在飞的读作废：
// 变更之前发起的读不会再把旧值回填进 L1，后续请求重新回源。收到其他实例的失效
// 广播时同样如此，因此开启广播的多实例部署一并受益。
//
// 上面那句"最多陈旧 localTTL"因此主要约束一种情形：未开启失效广播的多实例部署。
// 此时本实例无从得知其他实例的变更，只能等 L1 条目自然过期。
//
// DeletePattern 与按模式的失效广播无法反查在飞读对应的 key，只能逐分片作废，
// 各分片之间不是原子的，因此按模式变更之后，正在执行的那次读仍可能把变更前的值
// 回填进 L1，存活至多 localTTL。
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

	guards [twoLevelGuardShards]twoLevelKeyGuard
}

// guard 返回 key 所属分片的代次守卫。
func (s *TwoLevelStore) guard(key string) *twoLevelKeyGuard {
	return &s.guards[maphash.String(twoLevelGuardSeed, key)%twoLevelGuardShards]
}

// bumpAllGuards 递增全部分片代次，用于无法反查受影响 key 的按模式变更。
func (s *TwoLevelStore) bumpAllGuards() {
	for i := range s.guards {
		_ = s.guards[i].commitMutation(nil)
	}
}

// twoLevelGuardShards 是代次守卫的分片数。
//
// 定长数组而不是按 key 的 map：内存固定（约 4KB/实例）、无需淘汰，也就没有
// "淘汰计数器使其归零、从而让陈旧回填看起来合法"这个二阶问题。
// 代价是同分片的不同 key 会互相干扰——对 key X 的变更会抑制同分片 key Y 的一次
// 并发回填。抑制是安全方向的（最坏结果是下次多读一次 Redis），而干扰窗口只有
// 一次 Redis 往返，256 分片下很少发生。
const twoLevelGuardShards = 256

// twoLevelGuardSeed 只用于算分片下标，与安全无关。
var twoLevelGuardSeed = maphash.MakeSeed()

// twoLevelKeyGuard 为 L1 写入提供顺序纪律：代次只在持锁时读写，因此不需要 atomic。
//
// 单靠代次校验关不掉窗口。设读者为 A(取代次) → B(读远端) → C(校验) → D(写 L1)，
// 变更者为 M(写远端) → N(递增代次) → P(写 L1)：危险结果"D 落在 P 之后且写的是
// 陈旧值"要求 B < M；若 C 也早于 M，校验会通过，随后 M、N、P 全部完成，最后 D
// 才落地——陈旧值覆盖了新值。窗口只是从一次 Redis 往返缩小到 C 与 D 之间。
//
// 把 (C, D) 和 (N, P) 各自放进同一把锁，窗口就关闭了：C 在 N 之后取锁则看到代次
// 已变而跳过；C 在 N 之前取锁则 D 先写陈旧值、变更者随后写入新值，最终仍是新值。
// 关键是 N 与 P 同在一个临界区，读者插不进它们中间。
//
// 锁内不做网络 I/O：读远端与写远端都在临界区之外，锁只覆盖本地缓存的一次读写。
type twoLevelKeyGuard struct {
	mu         sync.Mutex
	generation uint64
}

// begin 取当前代次，供回填时校验。
func (g *twoLevelKeyGuard) begin() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.generation
}

// commitRead 在代次仍等于 generation、且这条 flight 未被超时释放时执行 write，
// 返回是否执行了。
//
// superseded 必须在锁内读，不能在调用前先 Load 一次：新旧 leader 之间没有发生变更
// 时代次相同，两个回填都会通过代次校验，旧的最后落地。锁内读则安全——superseded
// 在 Forget 之前置位，而新 leader 只可能在 Forget 之后产生，因此任何新 leader 的
// L1 写入都严格晚于 superseded=true。旧 leader 在新 leader 写入之后拿到锁必然看到
// 标记已置位；在之前拿到，它的陈旧写会被随后同样要拿锁的新 leader 覆盖。
func (g *twoLevelKeyGuard) commitRead(generation uint64, superseded *atomic.Bool, write func()) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.generation != generation || superseded.Load() {
		return false
	}
	write()
	return true
}

// commitMutation 递增代次，并在同一临界区内执行 write（可为 nil，表示只递增）。
func (g *twoLevelKeyGuard) commitMutation(write func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.generation++
	if write == nil {
		return nil
	}
	return write()
}

// TwoLevelStoreOption 用于配置 TwoLevelStore。
type TwoLevelStoreOption func(*TwoLevelStore)

// TwoLevelLogger 是 TwoLevelStore 用于记录后台失效广播错误的最小日志接口。
type TwoLevelLogger interface {
	Errorf(format string, args ...any)
}

// WithLocalTTL 设置本地缓存默认 TTL。负数入参被忽略，保持默认值。
//
// 零值会被 NewTwoLevelStore 拒绝：L1 的陈旧上限就是这个 TTL，取 0 会让 L1 条目
// 永不过期也永不被回收，Redis 条目过期后本实例将永久返回旧值。
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
//
// 自定义实现需要自行保证可被多个 goroutine 并发调用。
// 它的 Set 与 Delete 会在按 key 分片的临界区内被调用（见 TwoLevelStore 的顺序
// 纪律），因此这两个方法应尽快返回，不要在其中做网络 I/O 或长时间阻塞。
//
// local 为 nil（含装进接口的 typed-nil）时在构造期 panic：typed-nil 能绕过
// "未注入则用默认实现"的判断，放过去等于第一次读写时 nil 解引用。
func WithLocalStore(local LocalStore) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		if isNilValue(local) {
			panic("gincache: local store must not be nil")
		}
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
//
// client 为 nil（含装进接口的 typed-nil）或 channel 为空时在构造期 panic。
// 此前这两种输入是静默禁用广播：调用方以为开了广播、实际没开，多实例的 L1 会一直
// 陈旧到 localTTL 且没有任何信号。typed-nil 更是会一路走到 Subscribe 才 nil 解引用。
//
// 需要按条件启用时用 nil Option，构造函数会跳过它：
//
//	var opt persist.TwoLevelStoreOption
//	if broadcastEnabled {
//		opt = persist.WithTwoLevelInvalidationBroadcast(client, "app:l1:invalidate")
//	}
//	store := persist.NewTwoLevelStore(client, opt)
func WithTwoLevelInvalidationBroadcast(client redis.UniversalClient, channel string) TwoLevelStoreOption {
	return func(s *TwoLevelStore) {
		if isNilValue(client) {
			panic("gincache: invalidation broadcast client must not be nil")
		}
		if channel == "" {
			panic("gincache: invalidation broadcast channel must not be empty")
		}
		s.invalidationClient = client
		s.invalidation = &twoLevelInvalidation{channel: channel}
	}
}

// NewTwoLevelStore 创建一个 L1 可插拔、L2 为 Redis 的两级缓存。
//
// 配置错误在这里就地 panic，而不是拖到第一次读写时才以 nil 解引用的形式暴露：
// redisClient 为 nil（含装进接口的 typed-nil）即 panic，localTTL 为零值即 panic，
// opts 中的 nil 元素被跳过。
func NewTwoLevelStore(redisClient redis.Cmdable, opts ...TwoLevelStoreOption) *TwoLevelStore {
	// typed-nil 必须一起拦：`var c *redis.Client` 装进接口后接口本身非 nil，
	// 只查 `== nil` 会放它过去，第一次读取时在 singleflight 内 nil 解引用，
	// 连累所有等待者。
	if isNilValue(redisClient) {
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

	// 校验放在全部 Option 之后，判的是最终生效值：WithLocalTTL(0) 后面又跟一个
	// 正数的写法不该误报。
	//
	// 零值不能静默放过，也不能静默改写成默认值。localTTL 是 L1 的陈旧上限，取 0
	// 会让 MemoryStore 的 expiration 也是 0——条目永不过期，而后台清理只回收
	// expiration > 0 的条目，于是它永不被回收。结果是 Redis 条目过期后本实例永久
	// 返回旧值，且 L1 失去唯一的回收机制、内存只增不减。这类配置错误就地拒绝，
	// 不拖到运行期以"缓存好像不更新"的形式暴露。
	if s.localTTL == 0 {
		panic("gincache: localTTL must not be zero: omit WithLocalTTL to keep the default, or pass a positive duration")
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
		superseded, stopForget := s.scheduleSingleFlightForget(key)
		defer stopForget()

		var temp json.RawMessage
		if err := s.local.Get(key, &temp); err == nil {
			return twoLevelResult{data: temp, fromLocal: true}, nil
		}

		// 代次要在读远端之前取：回填时若代次已变，说明这次读依据的前提
		// （变更前的状态）已经失效，这份数据不该再写进 L1。
		guard := s.guard(key)
		generation := guard.begin()

		var data json.RawMessage
		remaining, err := s.remote.getWithTTL(key, &data)
		if err != nil {
			return nil, err
		}

		// 代次校验与"是否已被超时释放"都在 commitRead 的临界区内判定，
		// 与 L1 写入原子——两者都不能在临界区外先查一次。
		if backfill, ok := localBackfillTTL(s.localTTL, remaining); ok {
			guard.commitRead(generation, superseded, func() {
				_ = s.local.Set(key, data, backfill)
			})
		}

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

// localBackfillTTL 由配置的 localTTL 和远端剩余 TTL 算出回填时长，第二个返回值
// 为 false 表示这次不该回填。
//
// L1 条目不得活过对应的 L2 条目。写路径已经按 min(localTTL, 远端 TTL) 约束，
// 回填路径若照用完整 localTTL，一个只剩 1 秒的远端条目就能在 L1 里再活 30 秒——
// 事实来源过期之后的 29 秒里，本实例仍然返回它。
func localBackfillTTL(localTTL, remaining time.Duration) (time.Duration, bool) {
	if remaining == remoteTTLNoExpiry {
		return localTTL, true
	}

	// remoteTTLKeyMissing 是 GET 与 PTTL 之间远端条目消失了（过期或被删）。
	// 非正的剩余时间一并按此处理：已读到的值照样返回给本次调用方——它当时是
	// 有效的——但不给它一份新的 localTTL 寿命。
	if remaining <= 0 {
		return 0, false
	}

	return min(localTTL, remaining), true
}

// Set 写 Redis，成功后失效本地缓存。
func (s *TwoLevelStore) Set(key string, value any, expire time.Duration) error {
	return s.SetWithContext(context.Background(), key, value, expire)
}

// SetWithContext 写 Redis，成功后失效本地缓存，而不是把新值写进 L1。
// 返回值表达的是 Redis 的写入结果；本地失效失败只记录日志，不混入返回值——与
// Delete 一致。
//
// 写 L1 会让两级的最终值可能相反：远端写在守卫的临界区之外，两个并发 Set 的远端写
// 顺序与 L1 写顺序可以颠倒（实测得到 Redis=B 而 L1=A，两个调用都返回成功）。改为
// 失效之后，并发写都只删 L1，Redis 留下胜者，下次读从 Redis 回填并经代次校验，
// 两级必然一致。代价是该 key 的下一次读会穿到 Redis。
//
// 另一条出路是让守卫的锁横跨远端写，把整个变更按 key 串行化。那把每一次 Set 都压在
// 网络延迟上，代价远大于一次回源。
func (s *TwoLevelStore) SetWithContext(ctx context.Context, key string, value any, expire time.Duration) error {
	remoteExpire := expire
	if remoteExpire == 0 {
		remoteExpire = s.remoteTTL
	}

	if err := s.remote.SetWithContext(ctx, key, value, remoteExpire); err != nil {
		return err
	}

	// 变更已在事实来源生效，在飞的那次读是变更之前发起的，它的结果不该再分发给
	// 新加入的等待者。Forget 取消不了已在执行的回源（那个窗口见类型文档的陈旧
	// 上限），只是让后续请求另起 flight。远端失效消息的处理路径一直这么做，
	// 本地变更此前漏了，导致本地变更的陈旧扩散面反而大于远端变更。
	s.sf.Forget(key)

	s.publishInvalidation(ctx, twoLevelInvalidationKey, key)

	// 递增代次与失效 L1 同在一个临界区，且在远端写之后——顺序理由见 twoLevelKeyGuard。
	// 失效失败只记录日志：Redis 已经是新值，调用方的写确实成功了；L1 里留下的旧值
	// 最多活到 localTTL，把它报成写失败会误导调用方去重试一次已经成功的写入。
	if err := s.guard(key).commitMutation(func() error {
		return s.local.Delete(key)
	}); err != nil {
		s.logf("gincache: failed to invalidate local entry after remote set succeeded key=%q: %v", key, err)
	}

	return nil
}

// Delete 同时删除 Redis 和本地缓存中的单个 key。
// 返回值表达的是 Redis 的删除结果；本地删除失败只记录日志，不混入返回值。
func (s *TwoLevelStore) Delete(key string) error {
	// 先删远端再删本地：代次必须在远端变更之后才递增，而递增与删 L1 要在同一
	// 临界区内完成（理由见 twoLevelKeyGuard）。远端失败也要清 L1——留着旧值只会
	// 让本实例继续返回它。
	err := s.remote.Delete(key)

	if delErr := s.guard(key).commitMutation(func() error {
		return s.local.Delete(key)
	}); delErr != nil {
		s.logf("gincache: failed to delete local key=%q: %v", key, delErr)
	}
	s.sf.Forget(key)

	if err == nil {
		s.publishInvalidation(context.Background(), twoLevelInvalidationKey, key)
	}
	return err
}

// DeletePattern 先删除 Redis 中匹配模式的 key，再尽力清理本地缓存。
// 返回值表达的是 Redis 的删除结果；本地删除失败只记录日志，不混入返回值。
func (s *TwoLevelStore) DeletePattern(ctx context.Context, pattern string) (int64, error) {
	n, err := s.remote.DeletePattern(ctx, pattern)

	s.invalidateLocalPattern(ctx, pattern)

	if err == nil {
		s.publishInvalidation(ctx, twoLevelInvalidationPattern, pattern)
	}
	return n, err
}

// invalidateLocalPattern 递增全部分片代次，再删除本地匹配 key。
//
// 按模式删除无法反查受影响的 key，只能逐分片递增，让"取代次早于本次删除"的在飞
// 读取被拦下。各分片之间不是原子的，因此按模式变更的窗口不完全封闭——见
// TwoLevelStore 的类型文档。
func (s *TwoLevelStore) invalidateLocalPattern(ctx context.Context, pattern string) {
	s.bumpAllGuards()

	local, ok := s.local.(LocalStoreWithPattern)
	if !ok {
		return
	}
	if _, err := local.DeletePattern(ctx, pattern); err != nil {
		s.logf("gincache: failed to delete local pattern=%q: %v", pattern, err)
	}
}

// InvalidateLocal 只删除本地缓存中的单个 key，并让该 key 在飞的读作废。
func (s *TwoLevelStore) InvalidateLocal(key string) {
	_ = s.guard(key).commitMutation(func() error {
		return s.local.Delete(key)
	})
	s.sf.Forget(key)
}

// InvalidateLocalPattern 在本地缓存支持按模式删除时，清理本地匹配 key，
// 并让在飞的读作废。
func (s *TwoLevelStore) InvalidateLocalPattern(pattern string) {
	s.invalidateLocalPattern(context.Background(), pattern)
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
		"local_hit":      countAsInt64(localHit),
		"remote_hit":     countAsInt64(remoteHit),
		"miss":           countAsInt64(miss),
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

// scheduleSingleFlightForget 为一次 flight 安排释放定时器。
//
// 返回的 superseded 在定时器触发后为真，表示这条 flight 已被释放、可能已有新
// leader 在跑。Forget 不取消本 flight，因此旧 leader 仍会跑完；它读到的值比新
// leader 的更旧，回填上去就是把新的盖成旧的。stop 在 flight 结束时停止定时器。
func (s *TwoLevelStore) scheduleSingleFlightForget(key string) (superseded *atomic.Bool, stop func()) {
	superseded = new(atomic.Bool)
	if s.sfForgetTimeout <= 0 {
		return superseded, func() {}
	}

	timer := time.AfterFunc(s.sfForgetTimeout, func() {
		// 先置位再 Forget，避免出现"新 leader 已可产生而旧 leader 尚未被标记"的瞬间。
		superseded.Store(true)
		s.sf.Forget(key)
	})
	return superseded, func() {
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

// newInvalidationOrigin 生成一个实例身份，用于让接收端识别并跳过自己发出的失效消息。
//
// 身份必须在所有实例之间唯一，此前用的是 fmt.Sprintf("%p", s)：那对进程内存布局
// 做了无依据的假设，一旦两个进程的地址相同，接收端会把对方的广播当成自己的而
// 跳过，陈旧的 L1 就留到 localTTL 自然过期——广播能力静默失效。它还把一个堆地址
// 原样发进 Pub/Sub 载荷，凡能订阅该 channel 的都读得到。随机身份两个问题都没有。
func newInvalidationOrigin() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func (s *TwoLevelStore) startInvalidation() {
	// 未启用广播。启用时 channel 与 client 都已在 Option 里校验过非空非 nil，
	// 这里不再重复判空。
	if s.invalidation == nil {
		return
	}

	origin, err := newInvalidationOrigin()
	if err != nil {
		// 与订阅确认失败同一形态：禁用广播、记日志，不引入新的失败语义。
		s.logf("gincache: failed to generate invalidation origin channel=%q: %v", s.invalidation.channel, err)
		s.invalidation = nil
		return
	}
	s.invalidation.origin = origin
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
			// 递增代次让本实例在飞的那次读不再回填：它读到的是变更前的值。
			// 广播开启的多实例部署因此一并受益。
			if err := s.guard(event.Value).commitMutation(func() error {
				return s.local.Delete(event.Value)
			}); err != nil {
				s.logf("gincache: failed to delete local key from invalidation key=%q: %v", event.Value, err)
			}
			s.sf.Forget(event.Value)
		case twoLevelInvalidationPattern:
			s.invalidateLocalPattern(context.Background(), event.Value)
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
