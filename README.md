# gincache

生产级 Gin HTTP 响应缓存中间件，基于 `chenyahui/gin-cache` 设计思路，使用最新依赖重写。

## ✅ 生产级特性

- **Go 1.26 兼容** - 使用最新标准库特性
- **最新依赖** - `github.com/redis/go-redis/v9 v9.7.3`
- **Singleflight 防击穿** - 高并发时只有一个请求穿透到后端
- **仅缓存 2xx 响应** - 可配置缓存其他状态码
- **Body 大小限制** - 防止大响应撑爆缓存
- **Context 超时控制** - 所有缓存操作支持超时
- **sync.Pool 复用** - ResponseWriter 对象池化
- **完善的统计** - 命中率、本地/远程命中统计
- **两级缓存** - 本地 + Redis，扛热点数据

## 安装

```bash
go get github.com/yourorg/gincache
```

## 快速开始

```go
package main

import (
    "time"
    "github.com/yourorg/gincache"
    "github.com/yourorg/gincache/persist"
    "github.com/gin-gonic/gin"
    "github.com/redis/go-redis/v9"
)

func main() {
    r := gin.Default()

    // Redis 存储
    redisClient := redis.NewClient(&redis.Options{
        Addr:     "localhost:6379",
        PoolSize: 100,
    })
    store := persist.NewRedisStore(redisClient,
        persist.WithKeyPrefix("myapp:cache:"),
    )

    // 按 URI 缓存
    r.GET("/products",
        gincache.CacheByRequestURI(store, time.Minute),
        handler,
    )

    r.Run(":8080")
}
```

## API

### 缓存中间件

```go
// 按完整 URI 缓存（含 query string）
gincache.CacheByRequestURI(store, expire, opts...)

// 按路径缓存（忽略 query string）
gincache.CacheByRequestPath(store, expire, opts...)

// 自定义策略
gincache.Cache(store, expire,
    gincache.WithCacheStrategyByRequest(func(c *gin.Context) (bool, gincache.Strategy) {
        return true, gincache.Strategy{
            CacheKey:      "custom:key",
            CacheDuration: 5 * time.Minute,
        }
    }),
)
```

### Option 配置

```go
// 缓存命中/未命中回调（用于监控）
gincache.WithOnHitCache(func(c *gin.Context) { ... })
gincache.WithOnMissCache(func(c *gin.Context) { ... })

// singleflight 超时（防止长尾请求阻塞）
gincache.WithSingleFlightForgetTimeout(5 * time.Second)

// 忽略 query 参数顺序
gincache.WithIgnoreQueryOrder()

// 最大缓存 Body 大小
gincache.WithMaxBodySize(1 << 20) // 1MB

// 自定义可缓存状态码
gincache.WithCacheableStatusCodes(200, 201, 304)

// 日志
gincache.WithLogger(logger)

// 禁用 singleflight
gincache.WithDisableSingleFlight()
```

### 存储后端

#### Redis 存储（推荐生产使用）

```go
import "github.com/redis/go-redis/v9"

client := redis.NewClient(&redis.Options{
    Addr:         "localhost:6379",
    PoolSize:     100,
    MinIdleConns: 20,
    DialTimeout:  5 * time.Second,
    ReadTimeout:  3 * time.Second,
    WriteTimeout: 3 * time.Second,
})

store := persist.NewRedisStore(client,
    persist.WithKeyPrefix("myapp:cache:"),
    persist.WithReadTimeout(2 * time.Second),
    persist.WithWriteTimeout(2 * time.Second),
)

// 按模式删除
ctx := context.Background()
store.DeletePattern(ctx, "products:*")
```

#### 两级缓存（高并发热点数据）

```go
store := persist.NewTwoLevelStore(redisClient,
    persist.WithLocalTTL(15 * time.Second),  // 本地 TTL
    persist.WithRemoteTTL(5 * time.Minute),  // Redis TTL
)
defer store.Close()

// 热点接口
r.GET("/hot/config", gincache.CacheByRequestPath(store, time.Minute), handler)

// 查看统计
stats := store.Stats()
// {local_hit, remote_hit, miss, total_hit_rate, local_hit_rate}
```

#### 内存存储（单实例/开发环境）

```go
store := persist.NewMemoryStore(time.Minute,
    persist.WithCleanupInterval(30 * time.Second),
)
defer store.Close()

// 统计
stats := store.Stats()
// {keys, expired, hit, miss, set, del, hit_rate}
```

## 生产环境最佳实践

### 1. Redis 连接池配置

```go
client := redis.NewClient(&redis.Options{
    Addr:         "localhost:6379",
    PoolSize:     100,              // 根据并发量调整
    MinIdleConns: 20,               // 保持最小空闲连接
    DialTimeout:  5 * time.Second,
    ReadTimeout:  3 * time.Second,
    WriteTimeout: 3 * time.Second,
    PoolTimeout:  4 * time.Second,  // 获取连接超时
})
```

### 2. 缓存键设计

```go
// 推荐：前缀 + 层级结构
prefix := "myapp:api:"

// 列表缓存（按完整 URI）
// myapp:api:/products?page=1&per_page=20

// 详情缓存（按路径）
// myapp:api:/products/123

// 自定义 Key（业务相关）
// myapp:api:orders:user:123:status:completed
```

### 3. 主动缓存失效

```go
func (s *ProductService) Update(ctx context.Context, id int64) error {
    // 更新数据库...

    // 删除详情缓存
    store.DeleteWithContext(ctx, "/products/"+strconv.FormatInt(id, 10))

    // 删除列表缓存（按模式）
    store.DeletePattern(ctx, "/products?*")

    return nil
}
```

### 4. 监控

```go
var hitCount, missCount atomic.Uint64

opts := []gincache.Option{
    gincache.WithOnHitCache(func(c *gin.Context) {
        hitCount.Add(1)
        // prometheus.CacheHitTotal.Inc()
    }),
    gincache.WithOnMissCache(func(c *gin.Context) {
        missCount.Add(1)
        // prometheus.CacheMissTotal.Inc()
    }),
}
```

### 5. 大响应处理

```go
// 限制缓存 Body 大小，防止大响应撑爆 Redis
gincache.WithMaxBodySize(1 << 20) // 1MB

// 超过限制的响应不会被缓存，但会正常返回
```

## 测试

```bash
# 启动示例服务
go run examples/main.go

# 测试接口
curl http://localhost:8080/api/v1/products?page=1
curl http://localhost:8080/api/v1/products/123
curl http://localhost:8080/admin/cache/stats
```

## License

MIT