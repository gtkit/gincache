package persist

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gtkit/json"
	"github.com/redis/go-redis/v9"
)

// RedisStore Redis 缓存存储（生产级实现）
type RedisStore struct {
	client       redis.Cmdable
	keyPrefix    string
	readTimeout  time.Duration
	writeTimeout time.Duration
}

// RedisStoreOption Redis 存储选项
type RedisStoreOption func(*RedisStore)

// WithKeyPrefix 设置 Key 前缀
func WithKeyPrefix(prefix string) RedisStoreOption {
	return func(s *RedisStore) {
		s.keyPrefix = prefix
	}
}

// WithReadTimeout 设置读取超时
func WithReadTimeout(timeout time.Duration) RedisStoreOption {
	return func(s *RedisStore) {
		s.readTimeout = timeout
	}
}

// WithWriteTimeout 设置写入超时
func WithWriteTimeout(timeout time.Duration) RedisStoreOption {
	return func(s *RedisStore) {
		s.writeTimeout = timeout
	}
}

// NewRedisStore 创建 Redis 存储
// 支持 *redis.Client, *redis.ClusterClient, *redis.Ring
//
// 分片型客户端（*redis.ClusterClient、*redis.Ring）下，DeletePattern 与 Stats 会遍历
// 全部节点扫描；批量删除逐 key 下发，key 跨 hash slot 也能正常删除。
//
// client 为 nil（含装进接口的 typed-nil）时就地 panic，而不是把 nil 解引用拖到
// 第一次读写才暴露。
func NewRedisStore(client redis.Cmdable, opts ...RedisStoreOption) *RedisStore {
	if isNilValue(client) {
		panic("gincache: redis client must not be nil")
	}

	s := &RedisStore{
		client:       client,
		keyPrefix:    "gincache:",
		readTimeout:  3 * time.Second,
		writeTimeout: 3 * time.Second,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *RedisStore) key(k string) string {
	return s.keyPrefix + k
}

// scanPattern 把调用方的 pattern 拼成 SCAN 用的 glob。
//
// 固定前缀必须先转义：WithKeyPrefix 接受任意字符串，前缀里的 * ? [ ] \ 会改变模式
// 含义——`app*:` 这样的前缀能让 DeletePattern 匹配到别的命名空间并把它删掉。调用方
// 传入的 pattern 保持原样，它的通配语义正是调用方要的。
//
// 单 key 路径（key 方法）不转义：那里的结果是字面 key，不经过 glob 匹配，转义反而
// 会寻址到错误的 key。
func (s *RedisStore) scanPattern(pattern string) string {
	return escapeGlobMeta(s.keyPrefix) + pattern
}

// escapeGlobMeta 转义 Redis glob 的元字符，使字符串只匹配自身。
func escapeGlobMeta(s string) string {
	if !strings.ContainsAny(s, `*?[]\`) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := range len(s) {
		switch c := s[i]; c {
		case '*', '?', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// Get 从 Redis 获取缓存
func (s *RedisStore) Get(key string, value any) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.readTimeout)
	defer cancel()

	data, err := s.client.Get(ctx, s.key(key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return ErrCacheMiss
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

// Redis PTTL 的两个哨兵返回值。go-redis 对它们直接返回 time.Duration(n) 而不乘
// 精度，因此判断必须用精确比较，不能按毫秒去推算。
const (
	remoteTTLKeyMissing = time.Duration(-2) // key 不存在
	remoteTTLNoExpiry   = time.Duration(-1) // key 存在但没有过期时间
)

// getWithTTL 一次往返同时取回值和剩余过期时间。
//
// 回填 L1 需要远端剩余 TTL，而 L2 命中是热路径：单独再发一次 TTL 会让往返翻倍，
// 所以把 GET 与 PTTL 合进一个 pipeline，往返次数与原来的单次 GET 相同。
//
// 剩余时间沿用 PTTL 的三态语义，见 remoteTTLKeyMissing / remoteTTLNoExpiry。
func (s *RedisStore) getWithTTL(key string, value any) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.readTimeout)
	defer cancel()

	full := s.key(key)
	pipe := s.client.Pipeline()
	get := pipe.Get(ctx, full)
	pttl := pipe.PTTL(ctx, full)

	// key 不存在时 Exec 返回 redis.Nil。那是 GET 这一条命令的结果，不是整批失败，
	// 各命令的结果仍要逐个取出，因此这里只放过 redis.Nil。
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return 0, err
	}

	data, err := get.Bytes()
	if errors.Is(err, redis.Nil) {
		return 0, ErrCacheMiss
	}
	if err != nil {
		return 0, err
	}

	remaining, err := pttl.Result()
	if err != nil {
		return 0, err
	}

	if err := json.Unmarshal(data, value); err != nil {
		return 0, err
	}
	return remaining, nil
}

// Set 设置 Redis 缓存
func (s *RedisStore) Set(key string, value any, expire time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.writeTimeout)
	defer cancel()
	return s.SetWithContext(ctx, key, value, expire)
}

// SetWithContext 带 Context 的设置缓存
func (s *RedisStore) SetWithContext(ctx context.Context, key string, value any, expire time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, s.key(key), data, expire).Err()
}

// Delete 删除 Redis 缓存
func (s *RedisStore) Delete(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.writeTimeout)
	defer cancel()
	return s.client.Del(ctx, s.key(key)).Err()
}

// DeleteWithContext 带 Context 删除缓存
func (s *RedisStore) DeleteWithContext(ctx context.Context, key string) error {
	return s.client.Del(ctx, s.key(key)).Err()
}

// eachNode 对每个持有数据的节点调用 fn。
//
// 分片型客户端必须逐节点执行无 key 命令（如 SCAN）：go-redis 的 ClusterClient 在命令
// 层只覆写了 DBSize，SCAN 走通用路由，而无 key 命令的 slot 是 -1（由 ShardPicker 选
// 一个节点），因此只会落到其中一个节点上。Ring 同理没有 Scan 覆写。
// redis.Cmdable 不暴露任何 ForEach*，只能按具体类型断言。
//
// 回调可能被并发调用（ForEachMaster / ForEachShard 都是并发的），fn 内的共享状态
// 必须自行同步。
//
// 测试覆盖：*redis.Ring 与单节点分支由 miniredis 真实覆盖（见
// TestRedisStoreRingFansOutAcrossShards）；*redis.ClusterClient 分支与 Ring 结构对称，
// 但本地没有真集群，未被测试覆盖。
func (s *RedisStore) eachNode(ctx context.Context, fn func(ctx context.Context, node redis.Cmdable) error) error {
	switch client := s.client.(type) {
	case *redis.ClusterClient:
		return client.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
			return fn(ctx, node)
		})
	case *redis.Ring:
		return client.ForEachShard(ctx, func(ctx context.Context, node *redis.Client) error {
			return fn(ctx, node)
		})
	default:
		return fn(ctx, s.client)
	}
}

// 两条扫描路径的批量大小。删除要拿着 key 做后续操作，批小些让每轮工作量可控；
// 统计只累加数量，批大些以减少往返。
const (
	deleteScanCount = 100
	statsScanCount  = 1000
)

// scanNode 在单个节点上按模式扫描，对每批扫到的 key 调用 fn。
//
// count 是 SCAN 的批量大小，由调用点决定——两条路径的取值不同，统一成一个值会让
// 其中一方要么多付往返、要么每轮工作量过大。
func scanNode(ctx context.Context, node redis.Cmdable, pattern string, count int64, fn func(keys []string) error) error {
	var cursor uint64
	for {
		keys, nextCursor, err := node.Scan(ctx, cursor, pattern, count).Result()
		if err != nil {
			return err
		}

		if len(keys) > 0 {
			if err := fn(keys); err != nil {
				return err
			}
		}

		cursor = nextCursor
		if cursor == 0 {
			return nil
		}

		// 检查 Context 是否取消
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}

// deleteEachKey 用 pipeline 逐 key 删除，返回实际删除的数量。
//
// 不用一条 DEL 带多个 key：Redis Cluster 要求多 key 命令的 key 同属一个 slot，
// go-redis 按首个 key 的 slot 路由且不做拆分，跨 slot 会得到 CROSSSLOT 错误。
// 一个 master 拥有多个 slot，所以即使只删同一节点扫出的 key 也可能跨 slot。
// pipeline 里每条命令只带一个 key，往返次数不变而任何客户端类型都安全。
func deleteEachKey(ctx context.Context, node redis.Cmdable, keys []string) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}

	pipe := node.Pipeline()
	cmds := make([]*redis.IntCmd, len(keys))
	for i, key := range keys {
		cmds[i] = pipe.Del(ctx, key)
	}
	_, execErr := pipe.Exec(ctx)

	// Exec 返回错误不代表所有命令都没执行——它给的是首个命令错误。因此无论 Exec
	// 结果如何都要把每条命令的结果取出来：已经删掉的必须计入，否则调用方拿到的
	// 删除数不真实，上层也分不清"部分成功"和"完全失败"（后者才不需要广播失效）。
	var deleted int64
	var firstCmdErr error
	for _, cmd := range cmds {
		n, err := cmd.Result()
		if err != nil {
			if firstCmdErr == nil {
				firstCmdErr = err
			}
			continue
		}
		deleted += n
	}

	if firstCmdErr != nil {
		return deleted, firstCmdErr
	}
	return deleted, execErr
}

// DeletePattern 按模式删除缓存（使用 SCAN，生产安全）。
// 分片型客户端（*redis.ClusterClient、*redis.Ring）会遍历全部节点。
func (s *RedisStore) DeletePattern(ctx context.Context, pattern string) (int64, error) {
	fullPattern := s.scanPattern(pattern)

	var deleted atomic.Int64
	err := s.eachNode(ctx, func(ctx context.Context, node redis.Cmdable) error {
		return scanNode(ctx, node, fullPattern, deleteScanCount, func(keys []string) error {
			n, err := deleteEachKey(ctx, node, keys)
			deleted.Add(n)
			return err
		})
	})
	return deleted.Load(), err
}

// DeleteKeys 批量删除指定的 keys。
// key 跨 hash slot 也能正常删除。
func (s *RedisStore) DeleteKeys(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}

	fullKeys := make([]string, len(keys))
	for i, k := range keys {
		fullKeys[i] = s.key(k)
	}

	_, err := deleteEachKey(ctx, s.client, fullKeys)
	return err
}

// Exists 检查 key 是否存在
func (s *RedisStore) Exists(ctx context.Context, key string) (bool, error) {
	n, err := s.client.Exists(ctx, s.key(key)).Result()
	return n > 0, err
}

// TTL 获取 key 的剩余过期时间
func (s *RedisStore) TTL(ctx context.Context, key string) (time.Duration, error) {
	return s.client.TTL(ctx, s.key(key)).Result()
}

// Client 获取底层 Redis 客户端
func (s *RedisStore) Client() redis.Cmdable {
	return s.client
}

// Ping 检查 Redis 连接
func (s *RedisStore) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}

// Stats 获取统计信息。
// 分片型客户端（*redis.ClusterClient、*redis.Ring）会统计全部节点。
//
// 代价与本前缀下的 key 总量成正比：它用 SCAN 遍历整个前缀，分片客户端下还要遍历每个
// 节点。**不适合高频轮询**，把它接到每秒抓取的监控端点上会持续给 Redis 加压。需要
// 常态化的命中率指标时，用 TwoLevelStore.Stats 的计数器字段（hit/miss 是内存计数，
// 与 key 总量无关），或自行降低采集频率。
//
// 扫描未能在读取超时内走完时，返回已统计到的部分数量而不报错——Stats 是可观测接口，
// 不因此整体失败，但这意味着大 keyspace 上的 keys 值可能偏小。
func (s *RedisStore) Stats() map[string]int64 {
	ctx, cancel := context.WithTimeout(context.Background(), s.readTimeout)
	defer cancel()

	// 统计当前前缀下的 key 数量。扫描出错时按已统计到的数量返回——Stats 是可观测
	// 接口，不因单个节点不可用而整体失败。
	var count atomic.Int64
	_ = s.eachNode(ctx, func(ctx context.Context, node redis.Cmdable) error {
		return scanNode(ctx, node, s.scanPattern("*"), statsScanCount, func(keys []string) error {
			count.Add(int64(len(keys)))
			return nil
		})
	})

	return map[string]int64{
		"keys": count.Load(),
	}
}
