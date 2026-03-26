# gincache

`gincache` 是一个面向 Gin 的 HTTP 响应缓存中间件库。

它的核心目标是：

- 保持中间件层足够轻
- 保持存储层接口清晰
- 允许按需接入更重的本地缓存实现

当前主模块内置的能力：

- `RedisStore`
- `MemoryStore`
- `TwoLevelStore`
- `LocalStore` 可插拔本地缓存接口

当前主模块**不再内置** `Ristretto` 适配代码。  
如果你需要 `Ristretto`，请使用可选子模块：

- `github.com/gtkit/gincache/persist/ristrettoadapter`

这样做的目的很直接：

- 主库继续保持轻量风格
- `TwoLevelStore` 保留扩展点
- 想用 `Ristretto` 的项目再显式引入

## 特性概览

- 支持按完整 URI 缓存
- 支持按路径缓存
- 支持动态缓存策略
- 默认只缓存 `2xx`
- 支持自定义可缓存状态码
- 内置 `singleflight` 防击穿
- 支持响应体大小限制
- 支持本地缓存 + Redis 的两级缓存
- 支持通过 `LocalStore` 接口扩展任意 L1 本地缓存

## 安装

### 1. 安装主模块

```bash
go get github.com/gtkit/gincache
```

### 2. 如果你要接入 Ristretto，再额外安装可选子模块

```bash
go get github.com/gtkit/gincache/persist/ristrettoadapter
```

## 设计定位

这个项目现在采用的是“轻量内核 + 可选扩展”的结构：

1. 主模块 `gincache`
只保留通用缓存中间件能力和最基本的存储实现。

2. 主模块 `persist`
只保留轻量、通用、无强外部依赖的存储抽象与实现。

3. 可选子模块 `persist/ristrettoadapter`
当你确实需要热点本地缓存、容量控制、内存上限时，再显式引入 `Ristretto`。

这比把 `Ristretto` 直接揉进主模块更符合轻量级 Go 库的风格。

## 项目目录结构

```text
gincache/
├─ cache.go
│  └─ Gin 缓存中间件核心实现
├─ cache_test.go
│  └─ 中间件回归测试
├─ version.go
│  └─ 版本号定义
├─ examples/
│  └─ main.go
│     └─ 示例服务
├─ persist/
│  ├─ store.go
│  │  └─ 通用缓存存储接口
│  ├─ local_store.go
│  │  └─ TwoLevelStore 使用的本地缓存抽象接口
│  ├─ memory.go
│  │  └─ 轻量内存缓存实现
│  ├─ redis.go
│  │  └─ Redis 缓存实现
│  ├─ twolevel.go
│  │  └─ 两级缓存实现，L1 可插拔，L2 固定为 Redis
│  ├─ twolevel_test.go
│  │  └─ 两级缓存回归测试
│  ├─ twolevel_localstore_test.go
│  │  └─ 本地缓存注入测试
│  └─ ristrettoadapter/
│     ├─ go.mod
│     │  └─ 可选子模块，单独维护依赖
│     ├─ adapter.go
│     │  └─ Ristretto 适配器实现
│     └─ adapter_test.go
│        └─ Ristretto 适配器测试
├─ docs/
│  └─ superpowers/
│     ├─ specs/
│     └─ plans/
├─ go.mod
├─ go.sum
└─ README.md
```

## 快速开始

下面是最简单的 Redis 缓存用法：

```go
package main

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gtkit/gincache"
	"github.com/gtkit/gincache/persist"
	"github.com/redis/go-redis/v9"
)

func main() {
	r := gin.Default()

	// 创建 Redis 客户端
	redisClient := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379",
	})

	// 创建 Redis 缓存存储
	store := persist.NewRedisStore(redisClient,
		persist.WithKeyPrefix("demo:cache:"),
	)

	// 给接口挂上缓存中间件
	r.GET("/products",
		gincache.CacheByRequestURI(store, time.Minute),
		func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{
				"data": []string{"p1", "p2"},
				"time": time.Now().Unix(),
			})
		},
	)

	_ = r.Run(":8080")
}
```

## 中间件使用方式

### 1. `CacheByRequestURI`

按完整 URI 缓存，适合：

- 列表接口
- 搜索接口
- 分页接口

```go
r.GET("/products",
	gincache.CacheByRequestURI(store, time.Minute),
	handler,
)
```

### 2. `CacheByRequestPath`

按路径缓存，适合：

- 详情接口
- query 参数不影响结果的接口

```go
r.GET("/products/:id",
	gincache.CacheByRequestPath(store, 5*time.Minute),
	handler,
)
```

### 3. `Cache + WithCacheStrategyByRequest`

适合需要动态决定缓存 key、TTL、是否缓存的场景。

```go
r.GET("/orders",
	gincache.Cache(store, time.Minute,
		gincache.WithCacheStrategyByRequest(func(c *gin.Context) (bool, gincache.Strategy) {
			status := c.Query("status")
			userID := c.GetHeader("X-User-ID")

			// 这里只缓存已完成订单
			if status != "completed" {
				return false, gincache.Strategy{}
			}

			return true, gincache.Strategy{
				CacheKey:      "orders:user:" + userID + ":status:" + status,
				CacheDuration: 2 * time.Minute,
			}
		}),
	),
	handler,
)
```

## 中间件 Option 说明

### `WithCacheStrategyByRequest`

动态决定：

- 是否缓存
- 缓存 key
- 缓存存储
- 缓存 TTL

### `WithOnHitCache`

缓存命中回调，适合做指标统计。

### `WithOnMissCache`

缓存未命中回调，适合做 miss 统计。

### `WithOnShareSingleFlight`

请求命中 `singleflight` 共享结果时触发。

### `WithSingleFlightForgetTimeout`

用于控制 `singleflight.Forget` 的超时时间。

### `WithIgnoreQueryOrder`

忽略 query 参数顺序。

例如：

- `?a=1&b=2`
- `?b=2&a=1`

会被视为同一个缓存 key。

### `WithMaxBodySize`

限制可缓存响应体大小。

```go
gincache.WithMaxBodySize(1 << 20) // 1MB
```

### `WithCacheableStatusCodes`

自定义允许缓存的状态码。

默认只缓存 `2xx`。

### `WithDisableSingleFlight`

关闭请求去重。  
除非你明确知道自己在做什么，否则不建议在生产环境关闭。

## 存储层使用方式

## 1. RedisStore

这是最推荐的共享缓存实现。

```go
redisClient := redis.NewClient(&redis.Options{
	Addr:         "127.0.0.1:6379",
	PoolSize:     100,
	MinIdleConns: 20,
	DialTimeout:  5 * time.Second,
	ReadTimeout:  3 * time.Second,
	WriteTimeout: 3 * time.Second,
	PoolTimeout:  4 * time.Second,
})

store := persist.NewRedisStore(redisClient,
	persist.WithKeyPrefix("myapp:api:"),
	persist.WithReadTimeout(2*time.Second),
	persist.WithWriteTimeout(2*time.Second),
)
```

常见操作：

```go
ctx := context.Background()

// 删除单个 key
_ = store.Delete("/products/100")

// 按模式删除
_, _ = store.DeletePattern(ctx, "/products:*")

// 查询是否存在
ok, _ := store.Exists(ctx, "/products/100")

// 获取剩余 TTL
ttl, _ := store.TTL(ctx, "/products/100")

_ = ok
_ = ttl
```

## 2. MemoryStore

适合：

- 单实例
- 开发环境
- 本地调试

```go
store := persist.NewMemoryStore(time.Minute,
	persist.WithCleanupInterval(30*time.Second),
)
defer store.Close()
```

注意：`MemoryStore` 没有容量上限，不适合高基数、内存敏感场景。

## 3. TwoLevelStore

`TwoLevelStore` 的设计是：

- L1：本地缓存
- L2：Redis

读取流程：

1. 先查本地缓存
2. 本地未命中再查 Redis
3. Redis 命中后回填本地缓存

写入流程：

1. 先写 Redis
2. Redis 成功后再填本地缓存

这样可以避免“本地有值但 Redis 写失败”的不一致问题。

### 默认用法

默认 L1 是 `MemoryStore`。

```go
store := persist.NewTwoLevelStore(redisClient,
	persist.WithLocalTTL(30*time.Second),
	persist.WithRemoteTTL(5*time.Minute),
	persist.WithTwoLevelKeyPrefix("myapp:hot:"),
)
defer store.Close()
```

### 自定义注入本地缓存

主模块保留了 `LocalStore` 扩展点。

```go
local := persist.NewMemoryStore(30 * time.Second)

store := persist.NewTwoLevelStore(redisClient,
	persist.WithLocalStore(local),
	persist.WithRemoteTTL(5*time.Minute),
)
```

### 查看统计

```go
stats := store.Stats()
// 包含：
// local_hit
// remote_hit
// miss
// total_hit_rate
// local_hit_rate
```

## 可选模块：Ristretto 接入

`Ristretto` 适配器已经从主模块拆出，放在：

- `github.com/gtkit/gincache/persist/ristrettoadapter`

这样主模块不会内置 `Ristretto` 代码，只有需要的人才引入。

### 安装

```bash
go get github.com/gtkit/gincache/persist/ristrettoadapter
```

### 方案 A：先创建适配器实例，再注入 `TwoLevelStore`

这是最清晰的接法。

```go
package main

import (
	"time"

	ristretto "github.com/dgraph-io/ristretto/v2"
	"github.com/gtkit/gincache/persist"
	"github.com/gtkit/gincache/persist/ristrettoadapter"
)

func buildStore(redisClient redis.Cmdable) *persist.TwoLevelStore {
	cache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: 1_000_000, // 计数器数量，通常建议是预估条目数的 10 倍左右
		MaxCost:     256 << 20, // 最大总成本，这里按 256MB 估算
		BufferItems: 64,        // 官方默认建议值
	})
	if err != nil {
		panic(err)
	}

	local := ristrettoadapter.New(cache,
		ristrettoadapter.WithDefaultExpiration(30*time.Second),
		ristrettoadapter.WithCost(func(_ string, b []byte) int64 {
			return int64(len(b))
		}),
	)

	return persist.NewTwoLevelStore(redisClient,
		persist.WithLocalStore(local),
		persist.WithRemoteTTL(5*time.Minute),
	)
}
```

### 方案 B：直接把现成 `Ristretto` 实例转成 `TwoLevelStoreOption`

这是最方便的接法。

```go
cache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
	NumCounters: 1_000_000,
	MaxCost:     256 << 20,
	BufferItems: 64,
})
if err != nil {
	panic(err)
}

store := persist.NewTwoLevelStore(redisClient,
	persist.WithLocalTTL(30*time.Second),
	persist.WithRemoteTTL(5*time.Minute),
	ristrettoadapter.WithLocalStore(cache),
)
defer store.Close()
```

### 方案 C：在 `MemoryStore` 和 `Ristretto` 之间灵活切换

如果你要默认轻量：

```go
store := persist.NewTwoLevelStore(redisClient,
	persist.WithLocalTTL(30*time.Second),
)
```

如果你要换成 `Ristretto`：

```go
cache, _ := ristretto.NewCache(&ristretto.Config[string, []byte]{
	NumCounters: 1_000_000,
	MaxCost:     256 << 20,
	BufferItems: 64,
})

store := persist.NewTwoLevelStore(redisClient,
	persist.WithLocalTTL(30*time.Second),
	ristrettoadapter.WithLocalStore(cache),
)
```

业务层调用方式不变，变化只发生在缓存构造阶段。

## Ristretto 使用建议

推荐在这些场景使用：

- 热点明显
- 希望限制本地缓存内存上限
- 希望每个实例都有容量受控的热点层
- 访问模式不适合无界 `sync.Map`

不建议把它直接当成唯一缓存：

- 需要跨实例强一致
- 不能接受最终一致的本地缓存语义
- 业务强依赖写后全节点立即可见

### `WithWait()` 要不要开

默认建议：

- 先不开

原因：

- `Ristretto` 本身偏吞吐优化
- 不等待通常性能更好

适合开启的情况：

- 你希望测试行为更稳定
- 你希望本地写入后尽快可见
- 你更重视可见性而不是极限吞吐

## 完整示例

```go
package main

import (
	"net/http"
	"time"

	ristretto "github.com/dgraph-io/ristretto/v2"
	"github.com/gin-gonic/gin"
	"github.com/gtkit/gincache"
	"github.com/gtkit/gincache/persist"
	"github.com/gtkit/gincache/persist/ristrettoadapter"
	"github.com/redis/go-redis/v9"
)

func main() {
	r := gin.New()
	r.Use(gin.Recovery())

	redisClient := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:6379",
		PoolSize:     100,
		MinIdleConns: 20,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})

	localCache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
		NumCounters: 1_000_000,
		MaxCost:     256 << 20,
		BufferItems: 64,
	})
	if err != nil {
		panic(err)
	}

	store := persist.NewTwoLevelStore(redisClient,
		persist.WithLocalTTL(30*time.Second),
		persist.WithRemoteTTL(5*time.Minute),
		persist.WithTwoLevelKeyPrefix("myapp:hot:"),
		ristrettoadapter.WithLocalStore(localCache),
	)
	defer store.Close()

	r.GET("/hot/config",
		gincache.CacheByRequestPath(store, 5*time.Minute,
			gincache.WithMaxBodySize(1<<20), // 限制缓存体积，避免大对象进入缓存
		),
		func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{
				"feature_flag": true,
				"version":      "v1",
				"ts":           time.Now().Unix(),
			})
		},
	)

	_ = r.Run(":8080")
}
```

## 缓存失效建议

建议业务更新数据后主动失效相关缓存。

### 删除单个详情缓存

```go
func invalidateProductDetail(ctx context.Context, store *persist.RedisStore, id int64) error {
	return store.DeleteWithContext(ctx, "/api/v1/products/"+strconv.FormatInt(id, 10))
}
```

### 按模式删除列表缓存

```go
func invalidateProductList(ctx context.Context, store *persist.RedisStore) error {
	_, err := store.DeletePattern(ctx, "/api/v1/products*")
	return err
}
```

如果你使用的是 `TwoLevelStore`：

- Redis 删除负责全局共享缓存失效
- 本地缓存删除负责当前实例 L1 失效

当前主模块已经支持：

- `Delete(key)`
- `DeletePattern(pattern)`
- `InvalidateLocal(key)`
- `InvalidateLocalPattern(pattern)`

## 生产建议

推荐：

- L2 统一使用 Redis
- 热点场景优先考虑 `TwoLevelStore`
- L1 需要容量控制时，再显式接入 `Ristretto`
- 响应体较大时一定设置 `WithMaxBodySize`
- key 一定加业务前缀

不建议：

- 完全依赖 `MemoryStore` 却不做容量评估
- 多实例强一致场景却没有失效广播
- key 设计混乱导致无法精准失效

## 测试

### 主模块测试

```bash
go test ./...
go vet ./...
```

### Ristretto 适配器模块测试

```bash
cd persist/ristrettoadapter
go test ./...
```

## 当前状态

当前版本已经具备：

- 中间件并发共享响应测试
- 缓存回放测试
- 缓存状态码策略测试
- 两级缓存一致性测试
- 本地缓存注入测试
- `Ristretto` 适配器独立测试

仍需注意：

- 本地缓存仍是单实例语义
- 跨实例 L1 失效广播还没有做
- `go test -race ./...` 依赖本机有可用 C 编译器

## License

MIT
