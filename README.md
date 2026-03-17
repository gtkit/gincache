# gincache

高性能 Gin HTTP 响应缓存中间件，基于 `chenyahui/gin-cache` 设计思路，使用最新依赖重写。

## 特性

- ✅ **Go 1.26 兼容** - 使用最新标准库特性
- ✅ **最新依赖** - `github.com/redis/go-redis/v9`、`golang.org/x/sync`
- ✅ **Singleflight 防击穿** - 高并发时只有一个请求穿透到后端
- ✅ **仅缓存 2xx 响应** - 错误响应不会被缓存
- ✅ **自定义缓存策略** - 按请求动态决定缓存 Key 和 TTL
- ✅ **多种存储后端** - Memory、Redis、两级缓存
- ✅ **缓存回调** - 命中/未命中/共享结果回调
- ✅ **Query 参数排序** - 可选忽略参数顺序

## 安装

```bash
go get github.com/example/gincache
```

## 快速开始

```go
package main

import (
    "time"
    "github.com/example/gincache"
    "github.com/example/gincache/persist"
    "github.com/gin-gonic/gin"
    "github.com/redis/go-redis/v9"
)

func main() {
    r := gin.Default()

    // Redis 存储
    redisClient := redis.NewClient(&redis.Options{
        Addr: "localhost:6379",
    })
    store := persist.NewRedisStore(redisClient)

    // 按 URI 缓存
    r.GET("/products", 
        gincache.CacheByRequestURI(store, time.Minute),
        func(c *gin.Context) {
            c.JSON(200, gin.H{"data": "..."})
        },
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

// 自定义策略缓存
gincache.Cache(store, expire, 
    gincache.WithCacheStrategyByRequest(func(c *gin.Context) (bool, gincache.Strategy) {
        // 返回 (是否缓存, 缓存策略)
        return true, gincache.Strategy{
            CacheKey:      "custom:key",
            CacheDuration: 5 * time.Minute,
        }
    }),
)
```

### Option 配置

```go
// 缓存命中回调
gincache.WithOnHitCache(func(c *gin.Context) {
    log.Println("hit:", c.Request.RequestURI)
})

// 缓存未命中回调
gincache.WithOnMissCache(func(c *gin.Context) {
    log.Println("miss:", c.Request.RequestURI)
})

// singleflight 共享结果回调
gincache.WithOnShareSingleFlight(func(c *gin.Context) {
    log.Println("shared:", c.Request.RequestURI)
})

// singleflight 超时（防止长尾请求）
gincache.WithSingleFlightForgetTimeout(5 * time.Second)

// 忽略 query 参数顺序
gincache.WithIgnoreQueryOrder()
```

### 存储后端

#### 1. Redis 存储

```go
import "github.com/redis/go-redis/v9"

client := redis.NewClient(&redis.Options{
    Addr:         "localhost:6379",
    PoolSize:     50,
    MinIdleConns: 10,
})

store := persist.NewRedisStore(client,
    persist.WithKeyPrefix("myapp:cache:"),
)
```

#### 2. 内存存储

```go
store := persist.NewMemoryStore(time.Minute,
    persist.WithCleanupInterval(30 * time.Second),
)
defer store.Close()
```

#### 3. 两级缓存

```go
store := persist.NewTwoLevelStore(
    30 * time.Second,  // 本地缓存 TTL
    redisClient,
    persist.WithLocalTTL(10 * time.Second),
)
defer store.Close()
```

## 最佳实践

### 1. 选择合适的缓存策略

| 场景 | 推荐方法 | 说明 |
|-----|---------|-----|
| 列表接口 | `CacheByRequestURI` | 分页参数影响结果 |
| 详情接口 | `CacheByRequestPath` | 忽略无关参数 |
| 复杂条件 | `Cache` + 自定义策略 | 灵活控制 |

### 2. 主动缓存失效

```go
// 更新数据后删除相关缓存
func (s *ProductService) Update(id int64) error {
    // 更新数据库...
    
    // 删除详情缓存
    store.Delete("/products/" + strconv.FormatInt(id, 10))
    
    // 删除列表缓存（按模式）
    store.DeletePattern("/products?*")
    
    return nil
}
```

### 3. 监控缓存效果

```go
var hitCount, missCount uint64

opts := []gincache.Option{
    gincache.WithOnHitCache(func(c *gin.Context) {
        atomic.AddUint64(&hitCount, 1)
        // 上报 Prometheus metrics
    }),
    gincache.WithOnMissCache(func(c *gin.Context) {
        atomic.AddUint64(&missCount, 1)
    }),
}
```

### 4. 高并发场景用两级缓存

```go
// 本地缓存扛热点，Redis 扛容量
store := persist.NewTwoLevelStore(
    10 * time.Second,   // 本地 TTL 短
    redisClient,
)

// 热点接口
r.GET("/hot/config", gincache.CacheByRequestPath(store, time.Minute), handler)
```

## 依赖

```
github.com/gin-gonic/gin v1.10.0
github.com/redis/go-redis/v9 v9.7.3
golang.org/x/sync v0.10.0
```

## License

MIT
