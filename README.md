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

主模块不内置 BigCache、FreeCache、Ristretto 原生引擎。需要时按需引入一个独立适配模块：

- [BigCache](persist/bigcacheadapter/README.md)
- [FreeCache](persist/freecacheadapter/README.md)
- [Ristretto](persist/ristrettoadapter/README.md)

主库保留通用接口，调用方决定引擎，以及业务和 HTTP 使用独立实例还是共享实例。新 Owner API 的按需安装、完整示例和生命周期约束见下文“可选本地缓存模块”。

## 特性概览

- 支持按完整 URI 缓存
- 支持按路径缓存
- 支持动态缓存策略
- 内置中间件只缓存 `GET` / `HEAD`，并绕过携带 `Authorization` 的请求
- 默认缓存 `2xx`（排除 `206`）
- 支持自定义可缓存状态码
- 内置准入基线：默认拒绝缓存 `Set-Cookie`、`Cache-Control: no-store` / `private` / `no-cache`、`Vary: *` 的响应
- 写入 TTL 受响应声明的新鲜期约束（`s-maxage` / `max-age` / `Expires`），配置的 TTL 是上限
- 缓存命中时按 `If-None-Match` / `If-Modified-Since` 返回 `304`
- 支持自定义响应期缓存判据，写入、命中回放、singleflight 共享三个时机统一生效
- 内置 `singleflight` 防击穿
- 支持响应体大小限制
- 缓存命中时保留原始响应头（含多值 header），并剔除连接级 header
- 支持本地缓存 + Redis 的两级缓存
- 支持通过 `LocalStore` 接口扩展任意 L1 本地缓存

## 安装

### 1. 安装主模块

```bash
go get github.com/gtkit/gincache
```

### 2. 需要原生本地缓存时，选择一个独立适配模块

BigCache、FreeCache、Ristretto 分别独立分发，不需要同时引入。按下文“可选本地缓存模块”安装所选模块；需要源码联调时再使用本地 go.mod 的 replace 示例。已有 Ristretto 旧接口继续保留，见后文兼容用法。

## 设计定位

这个项目现在采用的是“轻量内核 + 可选扩展”的结构：

1. 主模块 `gincache`
只保留通用缓存中间件能力和最基本的存储实现。

2. 主模块 `persist`
只保留轻量、通用、无强外部依赖的存储抽象与实现。

3. 可选子模块 `persist/bigcacheadapter`、`persist/freecacheadapter`、`persist/ristrettoadapter`
当需要有界原生本地缓存时，调用方显式引入所选模块。共享实现留在 internal，原生引擎依赖留在各自的独立 module。

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
│  ├─ internal/
│  │  └─ localadapter/、adaptertest/：共享内核与测试辅助
│  ├─ bigcacheadapter/
│  │  └─ 独立 go.mod、Owner 适配、测试与 README
│  ├─ freecacheadapter/
│  │  └─ 独立 go.mod、Owner 适配、测试与 README
│  └─ ristrettoadapter/
│     └─ 独立 go.mod、旧 adapter.go、新 Owner API、测试与 README
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

#### 这两个中间件缓存哪些请求

只缓存 **`GET` 与 `HEAD`**，其余方法直接放行不缓存。缓存一次 `POST` / `PUT` / `DELETE`
的响应并回放，等于让后续同键请求跳过业务处理——副作用不会发生，调用方却收到成功响应。

**携带 `Authorization` 的请求整体绕过**，既不读也不写。内置缓存键不含任何请求头，无从
区分不同凭据的用户；RFC 9111 §3.5 也规定共享缓存不得复用带 `Authorization` 请求的响应。

需要缓存非安全方法或带凭据的响应，改用下面的 `Cache + WithCacheStrategyByRequest`，
在 `Strategy.CacheKey` 里自行拼入所需维度——那条路径不受这两条限制。

#### 这两个中间件的缓存键构成

缓存键为 `"<method> <uri>"` 或 `"<method> <path>"`。`GET` 与 `HEAD` **各用各的键**，
互不复用：`HEAD` 若归一到 `GET` 键上，一个"HEAD 分支不产出 Body"的普通 handler 只要
先被 HEAD 请求命中一次，就会把空条目写进 `GET` 键，随后的 `GET` 拿到空响应。

键中**不含任何请求头**——Cookie、`Accept-*` 都不在内。响应内容随请求头变化的接口
（典型信号是响应带 `Vary`）必须改用 `Cache + WithCacheStrategyByRequest` 自行拼键。

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

返回 `true` 时 `Strategy.CacheKey` 必须非空。空键会让本次请求跳过缓存（不读也不写），
并通过 `WithLogger` 配置的 Logger 记录一条错误日志——中间件不会替你回落到 URI，
因为它无法得知键里本该拼入哪些维度（例如用户标识）。

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
当响应体超过阈值时，中间件会停止继续缓冲该响应，并直接跳过缓存写入；
客户端仍然会收到完整响应。

```go
gincache.WithMaxBodySize(1 << 20) // 1MB
```

### `WithCacheableStatusCodes`

自定义允许缓存的状态码。

默认缓存 `2xx`，但排除 `206 Partial Content`——缓存键不含 `Range`，回放会把某一段字节
连同 `Content-Range` 当成完整响应发给所有人。显式配置优先：你给出的集合会被原样尊重。

### `WithCacheableResponse`

设置响应期缓存判据，判定一个**已经产生的响应**能否被共享。

请求期判据（`WithCacheStrategyByRequest`）跑在 handler 之前，那时响应头还不存在，
因此无法表达"这个响应不该被共享缓存"。本包会把响应头连同 Body 一起缓存并在命中时
回放，所以带 `Set-Cookie` 或 `Cache-Control: private` 的响应一旦进了缓存就是会话泄漏。

判据在三个时机被调用，返回 `false` 一律表示"不共享"：

| 时机 | 返回 false 的效果 |
|------|------------------|
| 写入缓存前 | 跳过缓存，本次响应照常发给客户端 |
| 缓存命中回放前 | 视为未命中，继续执行后续 handler |
| singleflight 把 leader 的响应交给并发等待者前 | 等待者改为执行自己的 handler |

因此判据应保持廉价且无副作用，尤其不要在里面计数或打点——同一次请求可能调用两次。
传给判据的是 header 的副本，改它不会影响写入 store 或即将回放的内容。

#### 内置基线 `DefaultCacheableResponse`

不设 `WithCacheableResponse` 时，内置基线默认生效，拒绝这些响应：

- 携带任意 `Set-Cookie` 的响应
- `Cache-Control` 指令中出现 `no-store` 的响应
- `Cache-Control` 指令中出现 `private` 的响应
- `Cache-Control` 指令中出现 `no-cache` 的响应
- `Vary` 中出现 `*` 的响应

指令按逗号切分后逐个比对、大小写不敏感、不做子串匹配，`no-store-hint` 之类的自定义
指令不会被误判。

`no-cache` 的语义是"未经重新验证不得复用"而不是"不得存储"。本包没有 revalidation
机制，存了就必然无验证复用——"允许存储"这一半没有任何可利用的余地，因此对本包
而言 `no-cache` 等价于不可缓存。

响应头的键会在写入与回放前统一规范化。`http.Header` 底层是 map，`Header.Set` 会规范化
键而直接写 map 不会；不归一的话，一句 `c.Writer.Header()["set-cookie"] = ...` 就能让
`Set-Cookie` 绕过上面这些检查。

`WithCacheableResponse` **替换**基线而不是叠加。替换是为了保留一个合法用法：缓存键里
已经带了用户维度时，缓存 `Cache-Control: private` 的响应是正确的。需要"基线加上自己的
条件"时显式组合：

```go
gincache.WithCacheableResponse(func(status int, header http.Header) bool {
    return gincache.DefaultCacheableResponse(status, header) &&
        header.Get("X-No-Cache") == ""
})
```

需要恢复"只按状态码判定"的旧行为时，传一个恒为真的判据：

```go
gincache.WithCacheableResponse(func(int, http.Header) bool { return true })
```

注意：**响应声明的新鲜期不受这个判据影响**（见下节）。判据表达的是"这个响应能不能
共享"，是你的策略；新鲜期表达的是"能共享多久"，是响应自己的声明，两者正交。

#### 配置的 TTL 是上限

写入缓存使用的 TTL 取**配置值与响应声明的新鲜期中较小的一个**。新鲜期按共享缓存的
优先级依次取自 `Cache-Control: s-maxage`、`Cache-Control: max-age`、`Expires` 减 `Date`。

因此 `defaultExpire` 与 `Strategy.CacheDuration` 是 TTL 上限而不是最终值——handler 声明
`Cache-Control: max-age=60` 时，即使中间件配了 10 分钟，条目也只缓存 60 秒。

`defaultExpire` 传 **0** 表示"交由存储决定"，此时中间件不再约束 TTL，只保留"响应已经
陈旧就不缓存"这一道闸门。想让响应声明的新鲜期真正生效，必须给出一个显式的上限——
否则一个完全合法的 `max-age=31536000` 会把存储的默认 TTL 顶成一年。

新鲜期还会**扣除响应已经消耗掉的部分**（RFC 9111 §4.2.3 的 `current_age`，取 `now - Date`
与 `Age` 中较大者）。本地 handler 产生的响应没有 `Age`、`Date` 就是当下，这一项为 0；
只有 handler 在做上游代理并透传这两个头时才起作用——上游的 `max-age=60, Age: 60`
已经是陈旧响应，不扣就等于又给它续了 60 秒。

声明的新鲜期 ≤ 0 时**直接拒绝缓存**，这覆盖 `max-age=0`、`s-maxage=0`、已过期的 `Expires`，
以及无法解析的 `Expires`（含常见的 `Expires: 0`，按 RFC 9111 §5.3 视为过去时间）。
响应未声明新鲜期时，完全沿用配置的 TTL。

指令解析取保守值：同一个新鲜期指令出现多次且**取值不同**时按陈旧处理（取值相同的重复
不算冲突）；非法的 delta-seconds 按陈旧处理而不是当作没出现——`delta-seconds` 的语法是
纯数字，`max-age=+600`、`max-age=-1`、单边引号都属非法；超出可表示范围的值钳到最大值，
最终仍与配置 TTL 取小。

#### 命中回放会写出 `Age`

回放缓存条目时会写出 `Age`，表示该响应自源站生成以来的秒数。只扣本级 TTL 不够——下游的
CDN 或浏览器看到偏小的 `Age` 会把这份响应再多留一段时间。

`Age` 取两条估算中较大者：条目 `Date` 到现在的时长；条目记录的"收到响应时刻"到现在的
驻留时长加上"响应到达时已有的年龄"。两条都无法估算的条目（v1.3.0 之前写入、既无这两个
字段也无 `Date`）按未命中处理，由 handler 重新产生响应回填——每个键只需一次回源，而且被
singleflight 合并。

**新鲜度判定也用这个年龄**：回放前按 `freshness_lifetime > current_age` 判定，因此条目一旦
活过自己声明的新鲜期就不再被回放，哪怕存储的 TTL 还没到。`defaultExpire` 传 0 时尤其重要
——那时 TTL 由存储决定，可能远长于响应声明的新鲜期。

#### 请求的 `Cache-Control: no-store` 会被遵守

请求带 `no-store` 时，本次响应不写入缓存（RFC 9111 §5.2.1.5）；已有条目仍可正常命中。

这与上面"请求端 `no-cache` 不被遵守"不矛盾：`no-cache` 约束的是**复用**，遵守它等于开放
一个人人可用的缓存击穿入口；`no-store` 只约束**写入**，带它的请求制造不出别人的未命中。

#### 条件请求返回 304

缓存命中且条目为 `200` 时，`GET` / `HEAD` 请求的 `If-None-Match` 与 `If-Modified-Since`
会被评估，匹配则返回 `304 Not Modified` 且不发送 Body。`If-None-Match` 优先于
`If-Modified-Since`，比较采用弱比较（忽略 `W/` 前缀），`*` 匹配任何已缓存的表示。
多个 `If-None-Match` 字段行等价于一个列表，会一并评估；含逗号的合法 opaque-tag
（如 `"a,b"`）不会被当成两个 tag 切开。

`304` 响应不含 `Content-Type`、`Content-Length`、`Content-Encoding`；条目带 `ETag` 时也
不含 `Last-Modified`。其余缓存的响应头照常写出，`X-Cache: HIT` 保留。

#### 使用约束：具名 `Vary` 与范围请求

- **具名 `Vary` 不在基线拒绝清单内**（`Vary: *` 已默认拒绝）。缓存键不含它列出的请求头，
  因此响应带具名 `Vary` 时需要你把对应请求头纳入缓存键（用
  `Cache + WithCacheStrategyByRequest` 自己拼键），或者在判据里拒绝它。基线不默认拒绝具名
  `Vary`，是因为 `Vary: Origin` 来自任何 CORS 中间件、
  `Vary: Accept-Encoding` 来自任何压缩中间件，默认拒绝会把绝大多数项目的命中率打到 0；
  而它是否真的出错取决于压缩中间件挂在本中间件的外侧还是内侧，这是本包看不到的信息。
- **携带 `Range` 头的请求整体绕过缓存**，既不读也不写。
- **Handler 调用 `Hijack` 接管连接后（WebSocket 升级等）不产生缓存条目**，包装器观察不到
  写入时一律跳过缓存。
- **连接级 header 不进缓存也不回放**：`Connection`、`Proxy-Connection`、`Keep-Alive`、`TE`、
  `Trailer`、`Transfer-Encoding`、`Upgrade`、`Proxy-Authenticate`、`Proxy-Authorization`，
  以及 `Connection` 头值中列出的字段名。

#### 按请求头区分的响应必须自己拼键

内置中间件已经绕过带 `Authorization` 的请求，但缓存键仍然不含 Cookie、`Accept-*` 等
任何其他请求头。响应内容随这些请求头变化的接口，用内置中间件缓存会把第一个用户的
内容发给后续所有人。这类接口必须走 `Cache + WithCacheStrategyByRequest`，在
`Strategy.CacheKey` 里显式拼进相应维度：

```go
gincache.WithCacheStrategyByRequest(func(c *gin.Context) (bool, gincache.Strategy) {
    userID := c.GetString("user_id") // 由你的鉴权中间件写入
    return true, gincache.Strategy{CacheKey: "profile:" + userID}
})
```

#### 请求端的 `Cache-Control: no-cache` 不被遵守

客户端在请求里带 `Cache-Control: no-cache` 或 `Pragma: no-cache` 时，本包仍会回放缓存。
这是有意的：遵守它等于开放一个人人可用的缓存击穿入口，任何客户端加一个请求头就能强制
回源，对昂贵接口就是免费的 DoS——生产级缓存（nginx `proxy_cache`、各家 CDN）普遍出于
同样理由默认忽略它。

需要遵守的场景用现成的 `WithCacheStrategyByRequest` 表达即可，不必额外配置：

```go
gincache.WithCacheStrategyByRequest(func(c *gin.Context) (bool, gincache.Strategy) {
    if strings.Contains(c.GetHeader("Cache-Control"), "no-cache") {
        return false, gincache.Strategy{}
    }
    return true, gincache.Strategy{CacheKey: c.Request.URL.Path}
})
```

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

// 按模式删除（Redis glob）
_, _ = store.DeletePattern(ctx, "/products:*")

// 查询是否存在
ok, _ := store.Exists(ctx, "/products/100")

// 获取剩余 TTL
ttl, _ := store.TTL(ctx, "/products/100")

_ = ok
_ = ttl
```

说明：

- `DeletePattern` 使用 Redis glob 风格匹配
- `*` 表示任意长度
- `?` 表示任意单个字符
- 如果要匹配字面量 `?`，在 Go 字符串里请写成 `\\?`

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
如果 `WithCleanupInterval` 传入 `<= 0`，会自动回退到默认的 `1m`。

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

### L1 陈旧上限

L1 读到的数据最多陈旧 `localTTL`。这个上限由两条规则维持：

- 写入时 L1 的 TTL 取 `min(localTTL, 远端 TTL)`
- L2 命中回填时取 `min(localTTL, 远端剩余 TTL)`；远端条目已消失则不回填，远端没有过期时间则用完整 `localTTL`

因此 L1 条目不会活过对应的 L2 条目。需要更小的陈旧窗口就调小 `localTTL`；需要在变更后立刻收敛，用下面的失效广播让其他实例即时清理 L1。

写路径**失效** L1 而不是写入 L1：`Set` 成功后本实例该 key 的下一次读会穿到 Redis 并回填。写入 L1 会让两级的最终值可能相反——远端写在守卫的临界区之外，两个并发 `Set` 的远端写顺序与 L1 写顺序可以颠倒。代价是写多读少的 key 上 L1 命中率下降。

单个 key 的变更（`Set` / `Delete` / `InvalidateLocal`）会让该 key 在飞的读作废：变更之前发起的读不会再把旧值回填进 L1，后续请求重新回源。收到其他实例的失效广播时同样如此，因此开启广播的多实例部署一并受益。

所以"最多陈旧 `localTTL`"主要约束一种情形：**未开启失效广播的多实例部署**。此时本实例无从得知其他实例的变更，只能等 L1 条目自然过期。

`DeletePattern` 与按模式的失效广播无法反查在飞读对应的 key，只能逐分片作废，各分片之间不是原子的，因此按模式变更之后，正在执行的那次读仍可能把变更前的值回填进 L1，存活至多 `localTTL`。

### L1 容量责任与生命周期

两条运维约束，上线前请确认：

**容量由调用方负责。** 默认的 `persist.MemoryStore` 只依赖 TTL 回收条目，**没有条目数上限、没有淘汰策略**。峰值内存约等于：

```
一个 localTTL 窗口内出现过的不同 key 数 × 单条目大小
```

中间件的 `WithMaxBodySize` 只约束单条响应体大小，不约束条目数量。因此缓存键含高基数成分（查询参数、用户 ID、分页游标等）时，必须换成带容量上限的 L1：

```go
// 用 Ristretto（有 MaxCost）作为 L1
cache, err := ristretto.NewCache(&ristretto.Config[string, []byte]{
	NumCounters: 1e7,
	MaxCost:     256 << 20, // 256MB 上限
	BufferItems: 64,
})
if err != nil {
	return err
}

store := persist.NewTwoLevelStore(redisClient,
	persist.WithLocalTTL(30*time.Second),
	ristrettoadapter.WithLocalStore(cache, ristrettoadapter.WithOwnedCache()),
)
defer store.Close()
```

**必须调用 `Close`。** `persist.NewMemoryStore` 会启动后台清理协程，开启失效广播时还会有订阅协程。不调用 `Close` 会让它们一直存活——按租户、按配置动态创建 store 的用法尤其要注意。重复调用是安全的。

### 构造约束

以下入口在构造期就地拒绝 nil（含装进接口的 typed-nil），而不是把 nil 解引用推迟到第一次读写：

| 入口 | 拒绝的输入 |
|---|---|
| `persist.NewRedisStore` | `client` 为 nil |
| `persist.NewTwoLevelStore` | `redisClient` 为 nil；`localTTL` 为零值 |
| `persist.WithLocalStore` | `local` 为 nil |
| `persist.WithTwoLevelInvalidationBroadcast` | `client` 为 nil、`channel` 为空；未同时提供 `WithTwoLevelLogger` |
| `ristrettoadapter.New` / `ristrettoadapter.WithLocalStore` | `cache` 为 nil |
| `ristrettoadapter.WithCost` | 成本函数为 nil |

`ristrettoadapter.New(nil)` 尤其需要拦：Ristretto 的方法对 nil receiver 是安全的，包一个 nil 实例不会崩，而是静默变成黑洞缓存——每次 `Set` 返回 `nil` 装作写成功，每次 `Get` 返回未命中。

启用广播必须同时提供 `WithTwoLevelLogger`：订阅启动失败会让广播在该实例的生命周期内保持关闭，没有 logger 时这个降级毫无信号。

按条件启用广播时用 nil Option，构造函数会跳过它：

```go
var opt persist.TwoLevelStoreOption
if broadcastEnabled {
	opt = persist.WithTwoLevelInvalidationBroadcast(redisClient, "myapp:gincache:l1:invalidate")
}
store := persist.NewTwoLevelStore(redisClient, opt)
```

注入自定义 `LocalStore` 时注意：它的 `Set` 与 `Delete` 会在按 key 分片的临界区内被调用，应尽快返回，不要在其中做网络 I/O 或长时间阻塞。

### 分片型 Redis 客户端

`persist.RedisStore` 支持 `*redis.Client`、`*redis.ClusterClient`、`*redis.Ring`。分片型客户端下：

- `DeletePattern` 与 `Stats` 遍历全部节点扫描（Cluster 遍历全部 master，Ring 遍历全部 shard）。go-redis 对无 key 命令只路由到单个节点，不遍历，因此必须逐节点执行 `SCAN`。
- 批量删除逐 key 下发（一次 pipeline，往返次数不变），key 跨 hash slot 也能正常删除。一条 `DEL` 带多个跨 slot 的 key 会被 Redis Cluster 拒绝。

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

`WithLocalTTL` 必须传正数。传 0 会在构造时 panic：`localTTL` 就是 L1 的陈旧上限，取 0 会让 L1 条目永不过期也永不被回收，Redis 条目过期后本实例将永久返回旧值。想用默认值（30 秒）就省略这个 Option。负数被忽略，保持默认值。

### 多实例 L1 失效广播

对路由响应缓存这类数据，`TwoLevelStore` 可以开启 Redis Pub/Sub 广播：

```go
store := persist.NewTwoLevelStore(redisClient,
	persist.WithLocalTTL(30*time.Second),
	persist.WithRemoteTTL(5*time.Minute),
	persist.WithTwoLevelKeyPrefix("myapp:hot:"),
	persist.WithTwoLevelInvalidationBroadcast(redisClient, "myapp:gincache:l1:invalidate"),
	persist.WithTwoLevelInvalidationTimeout(5*time.Second),
)
defer func() {
	if err := store.Close(); err != nil {
		log.Printf("close cache store failed: %v", err)
	}
}()
```

开启后，`Set`、`Delete`、`DeletePattern` 成功时会发布 L1 失效消息。其他实例收到消息后会清理自己的本地缓存，下一次请求会从 Redis 重新读取并回填 L1。广播客户端必须是 `redis.UniversalClient`，这样 `Publish` 和 `Subscribe` 能力在编译期明确；普通 `redis.Cmdable` 仍可用于不开启广播的 `TwoLevelStore`。

这带来的主要效果是：多实例部署下，订单状态、价格、库存、后台配置等敏感路由缓存被更新或删除后，不再依赖默认 `30s` 本地 TTL 自然过期，通常可以在亚秒级收敛。它不会让缓存命中率变高，也不提供强一致保证；Redis Pub/Sub 断线期间的消息仍可能丢失，因此业务仍应把 Redis 作为 L2 事实来源。

建议配置日志以便排查广播链路问题：

```go
store := persist.NewTwoLevelStore(redisClient,
	persist.WithTwoLevelLogger(logger),
	persist.WithTwoLevelInvalidationBroadcast(redisClient, "myapp:gincache:l1:invalidate"),
)
```

`WithTwoLevelLogger` 会记录订阅启动失败、订阅异常关闭、消息解析失败、本地失效失败和发布失败。发布是同步执行的，能减少失效消息排队的不确定性，但会把 Redis Publish 的耗时计入 `Set` / `Delete` / `DeletePattern` 调用延迟。

如果某个 key 回源 Redis 时间过长，内部 singleflight 默认会在 `10s` 后 `Forget`，允许后续同 key 请求重新成为 leader。可以按业务延迟调整：

```go
store := persist.NewTwoLevelStore(redisClient,
	persist.WithTwoLevelSingleFlightForgetTimeout(3*time.Second),
)
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

### 接入其他本地缓存库

如果你想接入 `BigCache`、`FreeCache` 或其他本地缓存库，不需要改 `gincache` 主模块。调用方只要在自己的项目里实现 `persist.LocalStore`，再通过 `persist.WithLocalStore(...)` 注入 `TwoLevelStore` 即可。

`LocalStore` 的契约是：

```go
type LocalStore interface {
	Get(key string, value any) error
	Set(key string, value any, expire time.Duration) error
	Delete(key string) error
	Close() error
	Stats() map[string]int64
	ResetStats()
}
```

下面是一个 `BigCache` 适配器骨架，适合放在你的业务项目或独立扩展包里：

```go
package localcache

import (
	"errors"
	"time"

	"github.com/allegro/bigcache/v3"
	"github.com/gtkit/gincache/persist"
	"github.com/gtkit/json"
)

type BigCacheStore struct {
	cache *bigcache.BigCache
}

func NewBigCacheStore(cache *bigcache.BigCache) *BigCacheStore {
	return &BigCacheStore{cache: cache}
}

func (s *BigCacheStore) Get(key string, value any) error {
	data, err := s.cache.Get(key)
	if err != nil {
		if errors.Is(err, bigcache.ErrEntryNotFound) {
			return persist.ErrCacheMiss
		}
		return err
	}
	return json.Unmarshal(data, value)
}

func (s *BigCacheStore) Set(key string, value any, _ time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.cache.Set(key, data)
}

func (s *BigCacheStore) Delete(key string) error {
	return s.cache.Delete(key)
}

func (s *BigCacheStore) Close() error {
	return s.cache.Close()
}

func (s *BigCacheStore) Stats() map[string]int64 {
	return map[string]int64{"keys": int64(s.cache.Len())}
}

func (s *BigCacheStore) ResetStats() {}
```

使用时：

```go
cfg := bigcache.DefaultConfig(30 * time.Second)
cache, err := bigcache.New(context.Background(), cfg)
if err != nil {
	return err
}

store := persist.NewTwoLevelStore(redisClient,
	persist.WithLocalStore(localcache.NewBigCacheStore(cache)),
	persist.WithRemoteTTL(5*time.Minute),
)
defer func() {
	if err := store.Close(); err != nil {
		log.Printf("close cache store failed: %v", err)
	}
}()
```

注意：

- 上面示例里的 `expire` 参数没有单独传给 BigCache；BigCache 常见用法是通过 `DefaultConfig` 设置全局淘汰周期。
- 如果你需要每个 key 独立 TTL，可以在 adapter 写入的 value 里额外封装过期时间，并在 `Get` 时自行判断。
- 如果本地缓存库不支持按模式删除，`DeletePattern` 只会清理 Redis；本地 L1 依赖 TTL 或失效广播继续收敛。

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

## 可选本地缓存模块

主模块只依赖现有 `persist.CacheStore`（HTTP 存储）或 `persist.LocalStore`（两级缓存 L1）契约，不强制引入原生本地引擎。三个适配器分别是独立 Go module，调用方只选择需要的一个：

| 模块 | 原生引擎 | 模块版本 | 创建 / 借用 |
|---|---|---|---|
| [persist/bigcacheadapter](persist/bigcacheadapter/README.md) | BigCache v3.2.0 | v1.2.1 | `NewOwner(ctx, cfg)` / `WrapOwner(raw, cfg)` |
| [persist/freecacheadapter](persist/freecacheadapter/README.md) | FreeCache v1.2.7 | v1.2.1 | `NewOwner(capacityBytes)` / `WrapOwner(raw, capacityBytes)` |
| [persist/ristrettoadapter](persist/ristrettoadapter/README.md) | Ristretto v2.4.2 | v1.2.1 | `NewOwner(cfg)` / `WrapOwner(raw, cfg)`；旧 `New` 保留 |

这里的最小依赖指原生引擎之间不捆绑；主模块既有 Gin、Redis、JSON 依赖仍然存在。主模块版本为 v1.4.1，适配模块版本见上表。每个模块使用独立的版本标签。

### 按需引入与本地源码联调

以下只选择 FreeCache。示例目录为同级的 `cache-demo/` 与 `gincache/`；如果本机目录不同，将 `replace` 的右侧改为实际路径。这里只展示调用方的开发配置，不修改本仓正式 go.mod。

```text
work/
├── cache-demo/
└── gincache/
```

`cache-demo/go.mod`：

```go
module example.com/cache-demo

go 1.27

require (
	github.com/gin-gonic/gin v1.12.0
	github.com/gtkit/gincache v1.4.1
	github.com/gtkit/gincache/persist/freecacheadapter v1.2.1
)

replace github.com/gtkit/gincache v1.4.1 => ../gincache

replace github.com/gtkit/gincache/persist/freecacheadapter v1.2.1 => ../gincache/persist/freecacheadapter
```

上面的替换仅用于本地源码联调。使用发布版本时移除这些本地替换，再按需安装所选模块：

```bash
# 只安装所选模块，不需要安装另外两个适配器。
go get github.com/gtkit/gincache@v1.4.1 github.com/gtkit/gincache/persist/freecacheadapter@v1.2.1
```

本仓的 [go.work.example](go.work.example) 是另一种多模块本地开发入口，不会自动生效；从仓库根显式设置 `GOWORK="$PWD/go.work.example"` 后使用。该开发 workspace 同时列出三个模块，不代表调用方生产模块需要同时依赖三种引擎。

<a id="local-adapter-examples"></a>

### 独立实例和共享实例：完整示例

下面复制为 `main.go`，再从后面的三个 `driver.go` 中只选一个。独立与共享使用同一套调用代码，区别是创建两个 Owner 还是一个 Owner。业务和 HTTP 都使用不同 namespace。

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gtkit/gincache"
	"github.com/gtkit/gincache/persist"
)

func main() {
	shared := flag.Bool("shared", false, "业务与 HTTP 是否共享一个缓存实例")
	flag.Parse()
	if err := run(*shared); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, err); writeErr != nil {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run(shared bool) (err error) {
	ctx := context.Background()
	businessOwner, err := newOwner(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, businessOwner.Close(ctx)) }()

	httpOwner := businessOwner
	if !shared {
		httpOwner, err = newOwner(ctx)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, httpOwner.Close(ctx)) }()
	}

	business, err := newView(businessOwner, "demo.business.v1", time.Minute)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, business.Close()) }()
	responses, err := newView(httpOwner, "demo.http.v1", 30*time.Second)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, responses.Close()) }()

	// 同一逻辑键在两个 namespace 中互不覆盖；零 TTL 使用各自默认值。
	if err := business.Set("same-key", "business-value", 0); err != nil {
		return err
	}
	if err := responses.Set("same-key", "http-value", 0); err != nil {
		return err
	}
	businessValue, err := readText(business, "same-key")
	if err != nil {
		return err
	}
	httpValue, err := readText(responses, "same-key")
	if err != nil {
		return err
	}

	// 只演示公开、固定内容的 GET；不把个性化数据放进该路由。
	router := gin.New()
	handlerCalls := 0
	router.GET("/public/ping", gincache.Cache(responses, 30*time.Second,
		gincache.WithCacheableStatusCodes(http.StatusOK),
		gincache.WithMaxBodySize(4<<10),
		gincache.WithCacheableResponse(gincache.DefaultCacheableResponse),
		gincache.WithCacheStrategyByRequest(func(c *gin.Context) (bool, gincache.Strategy) {
			if c.GetHeader("Authorization") != "" || c.GetHeader("Cookie") != "" {
				return false, gincache.Strategy{}
			}
			return true, gincache.Strategy{
				CacheKey: c.Request.Method + " " + c.Request.URL.RequestURI(),
			}
		}),
	), func(c *gin.Context) {
		handlerCalls++
		c.String(http.StatusOK, "pong")
	})

	// 两次同步内存请求，不监听端口；返回后再关闭视图和 Owner。
	for range 2 {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/public/ping", nil)
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			return fmt.Errorf("unexpected HTTP status: %d", response.Code)
		}
	}
	_, err = fmt.Printf("shared=%t business=%s http=%s handler_calls=%d\n",
		shared, businessValue, httpValue, handlerCalls)
	return err
}

func readText(store *cacheView, key string) (string, error) {
	var value string
	if err := store.Get(key, &value); err != nil {
		if errors.Is(err, persist.ErrCacheMiss) {
			// 演示中打印标记；真实业务应在这里向数据源回源。
			return "(cache miss)", nil
		}
		return "", err
	}
	return value, nil
}
```

三个驱动文件是互斥的选择，不能同时放入同一个示例项目。变更驱动时同步修改 go.mod 的适配模块和对应 replace：三个适配模块统一使用 v1.2.1。

#### BigCache：driver.go

```go
package main

import (
	"context"
	"time"

	"github.com/allegro/bigcache/v3"
	"github.com/gtkit/gincache/persist/bigcacheadapter"
)

type cacheOwner = bigcacheadapter.Owner
type cacheView = bigcacheadapter.NamespaceStore

func newOwner(ctx context.Context) (*cacheOwner, error) {
	return bigcacheadapter.NewOwner(ctx, bigcache.Config{
		Shards:             16,
		LifeWindow:         10 * time.Minute,
		CleanWindow:        time.Minute,
		HardMaxCacheSize:   16, // 单位 MB，不是进程 RSS。
		MaxEntriesInWindow: 1024,
		MaxEntrySize:       512,
	})
}

func newView(owner *cacheOwner, namespace string, ttl time.Duration) (*cacheView, error) {
	return owner.NewStore(namespace, bigcacheadapter.WithStoreExpiration(ttl))
}
```

#### FreeCache：driver.go

```go
package main

import (
	"context"
	"time"

	"github.com/gtkit/gincache/persist/freecacheadapter"
)

type cacheOwner = freecacheadapter.Owner
type cacheView = freecacheadapter.NamespaceStore

func newOwner(_ context.Context) (*cacheOwner, error) {
	return freecacheadapter.NewOwner(16 << 20) // 每个实例 16 MiB。
}

func newView(owner *cacheOwner, namespace string, ttl time.Duration) (*cacheView, error) {
	return owner.NewStore(namespace, freecacheadapter.WithStoreExpiration(ttl))
}
```

#### Ristretto：driver.go

```go
package main

import (
	"context"
	"time"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/gtkit/gincache/persist/ristrettoadapter"
)

type cacheOwner = ristrettoadapter.Owner
type cacheView = ristrettoadapter.NamespaceStore

func newOwner(_ context.Context) (*cacheOwner, error) {
	return ristrettoadapter.NewOwner(ristretto.Config[string, []byte]{
		NumCounters: 100_000,
		MaxCost:     16 << 20,
		BufferItems: 64,
	})
}

func newView(owner *cacheOwner, namespace string, ttl time.Duration) (*cacheView, error) {
	return owner.NewStore(namespace, ristrettoadapter.WithStoreExpiration(ttl))
}
```

在演示项目目录执行以下命令：

```bash
GOWORK=off go mod tidy

# 两个 Owner、两个原生实例，业务和 HTTP 各自一份预算。
GOWORK=off go run . -shared=false

# 一个 Owner、一个原生实例，两个 namespace 共用预算。
GOWORK=off go run . -shared=true
```

示例通过两次同步内存 HTTP 请求演示 gincache，不监听端口；请求全部结束后才关闭资源。正常命中时 `handler_calls=1`，但缓存准入和淘汰允许 miss，不应把命中次数当成业务保证。`readText` 遇到 miss 只打印演示标记；真实业务应向数据源回源。发布检查已构建三种引擎的示例，并分别运行独立和共享两种模式。

### 独立与共享、键隔离

| 项目 | 独立实例：`-shared=false` | 共享实例：`-shared=true` |
|---|---|---|
| Owner / 原生实例 | 两个 | 一个 |
| 示例预算 | 每个实例一份，合计两份 | 共用一份 |
| 淘汰影响 | 相互独立 | 业务与 HTTP 可能相互挤占 |
| namespace | 分别设置 | 必须不同 |
| 关闭 | 分别关闭 Owner | 所有消费者退出后关闭一次 Owner |

- 视图实现现有 `persist.CacheStore` 和 `persist.LocalStore`，可以用于 HTTP 中间件或 TwoLevelStore 的 L1；也提供 `GetBytes` / `SetBytes`。
- 示例使用 `demo.business.v1` 和 `demo.http.v1`。namespace 须为非空合法 UTF-8、无 NUL、长度不超过 128 字节；同一 Owner 不允许重复绑定，关闭后也不复用该名称。
- 物理键按 `gac2:<namespace 字节长度>:<namespace>:<逻辑键>` 隔离，并在记录中保存完整键。业务与 HTTP 即使用相同逻辑键，也不代表缓存内容互通。
- 共享容量没有 namespace 硬配额。需要硬容量隔离时选择独立 Owner；共享实例也不会自动把业务失效转换成 HTTP 响应失效。
- 三种引擎都是进程内缓存。共享仅发生在同一个进程，不能让多个进程或 Pod 共用同一个本地实例。

### TTL 与错误语义

| 写入 TTL | 含义 |
|---|---|
| 正数 | 本次条目的逻辑 TTL，从写入调用入口计时，包含编码和等待消耗 |
| `0` | 回落到该视图的 `WithStoreExpiration`；未设置时默认 `0`，无逻辑期限 |
| 负数 | 返回 `ErrInvalidArgument` |

命中不续期；到达精确期限返回 `persist.ErrCacheMiss`，即使原生条目尚未回收。原生更早淘汰允许 miss，不能延长逻辑有效期。无逻辑期限不等于永不淘汰。

`SetBytes` 返回 `(stored, err)`，正常准入或容量拒绝可以是 `(false, nil)`。`Set` / `SetWithContext` 遇到这种拒绝也返回 nil，因此 nil 只表示没有真实写入错误，不保证下一次 Get 一定命中。`Get` / `Set` 使用 JSON 编解码，字节接口用于无需该层 JSON 编码的值。

### 生命周期与借用已有实例

`NewOwner` 创建并拥有原生实例；`WrapOwner` 只借用。每个原生对象只能包装成一个 Owner，再由该 Owner 分配多个 namespace，不能为了共享把同一裸对象分别 Wrap 两次。

1. 进程装配阶段创建 Owner 和视图；不要每个请求重新创建。
2. 运行中经视图读写，不绕过 Owner 直接写入、清空或关闭原生实例。
3. 关停时先停止新请求和后台生产者，等待所有缓存消费者退出。
4. `Store.Close()` 只关闭该视图，不关闭共享 Owner；创建方最后调用 `Owner.Close(ctx)`。
5. 使用 `WrapOwner` 时，Owner 关闭后原生对象仍由原创建方管理；全部使用者退出后按原生库的方式释放资源。

Owner 等待操作门时响应 context；已进入的原生同步调用、Wait 或 Close 不能被 context 强制打断。Close 超时不代表原生操作已经停止，不能据此提前关闭借用对象或宣称消费者已经退出。

### 容量选择与原生差异

| 引擎 | 预算含义 | 需要注意 |
|---|---|---|
| BigCache | 正数 HardMaxCacheSize，单位 MB | LifeWindow 辅助回收；索引和临时对象另占内存，大条目可能返回原生错误 |
| FreeCache | capacityBytes，至少 512 KiB | 单条键值约受总容量 1/1024 限制；需计入键、记录头和编码膨胀 |
| Ristretto | 正数 MaxCost，适配器按键和完整记录字节计费 | 准入可能拒绝；新 Owner 路径等待原生屏障并核对本次记录 |

示例均为每个实例约 16 MiB 的演示预算，独立模式有两份预算，共享模式共用一份。预算不等于进程 RSS；共享同一实例不能保证每个 namespace 的最低可用容量。原始 body 上限、序列化条目大小和原生预算必须分别考虑。具体约束见三个适配模块 README。

借用现有对象的入口分别为 `bigcacheadapter.WrapOwner(raw, originalConfig)`、`freecacheadapter.WrapOwner(raw, originalCapacityBytes)`、`ristrettoadapter.WrapOwner(raw, originalConfig)`；参数必须来自实际创建配置，不能通过假填配置改变已创建实例的容量。

<a id="gin-api-cache-integration"></a>

### gin_api 当前接入限制

以下描述的是已回退后的 gin_api，不是尚未实现的新接口：

- `internal/pkg/cache/cache.go` 的 `cache.New[T]` 仍使用自己的 map/LRU，并提供 GetOrLoad、singleflight、TTL 抖动和回源限制；不能直接把 namespace 视图赋给它。
- `internal/pkg/httpcache/cache.go` 的 `httpcache.New` 仍由配置选择原 Ristretto 或 Redis；`store: memory` 表示 Ristretto，不表示可以任选本地引擎。
- `internal/platform/responsecache` 已有内部 `Factory`，但默认 bootstrap 没有传入自定义工厂；`bootstrap.Options` 当前也没有公开的缓存工厂注入字段。
- 当前不存在 `store: bigcache` / `store: freecache` 配置。只修改 YAML 不能切换引擎，也不能让原业务缓存共享这些实例。

当前有效的 memory 配置例如：

```yaml
cache:
  enabled: true
  store: memory             # 当前仍是原 Ristretto 装配。
  ttl: 30s                  # 默认响应 TTL，路由可覆盖；配置上限为 10m。
  max_body_bytes: 1048576    # 原始响应体缓存上限，非原生条目的大小承诺。
  max_bytes: 67108864        # 当前 memory 后端的容量配置，单位字节。
```

若后续接入新适配器，应由调用方创建所选 Owner，并分别创建业务、HTTP namespace。HTTP 层需要接收 `persist.CacheStore` 的构造入口，复用现有 Stager 的身份绕过、Vary、响应准入和信封保护；bootstrap 需要把该入口接到资源生命周期中。原业务 `cache.Cache[T]` 若也要共享，需要另加可选存储注入并保留现有治理。直接改用视图的 Get/Set，并不等价于保留原 GetOrLoad 行为。

不需要在 gin_api 再建一个导入三种引擎的集中适配包。若保留原 Ristretto 默认实现后增加 BigCache/FreeCache，应用仍会同时依赖原 Ristretto 和所选引擎；若要求应用只包含一种引擎，还须调整默认装配并移除不再使用的原生依赖。以上都是后续代码改造边界，当前 gin_api 尚未提供这些注入入口。

## 旧 Ristretto 接口与 TwoLevelStore

下面的独立 Ristretto 模块继续保留旧接口兼容，使用它是可选项；自带适配的调用方不需要引入它。

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

本节仅适用于旧 `ristrettoadapter.New` 接口。新 `NewOwner` 路径已经执行写入屏障，不接受旧 `WithWait` Option。

默认建议：

- 先不开

原因：

- `Ristretto` 本身偏吞吐优化
- 不等待通常性能更好

适合开启的情况：

- 你希望测试行为更稳定
- 你希望本地写入后尽快可见
- 你更重视可见性而不是极限吞吐

补充：

- 如果你是把“外部创建好的 `Ristretto` 实例”交给 `ristrettoadapter.New(...)`
- 适配器内部维护的 tracked key 统计会基于当前缓存状态做惰性清理
- 如果你希望写入后的 tracked 状态更快收敛，建议配合 `WithWait()`

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
	_, err := store.DeletePattern(ctx, "/api/v1/products\\?*")
	return err
}
```

如果你使用的是 `TwoLevelStore`：

- Redis 删除负责全局共享缓存失效
- 本地缓存删除负责当前实例 L1 失效
- 开启 `WithTwoLevelInvalidationBroadcast` 后，其他实例的 L1 也会收到广播并清理

当前主模块已经支持：

- `Delete(key)`
- `DeletePattern(pattern)`
- `InvalidateLocal(key)`
- `InvalidateLocalPattern(pattern)`
- `WithTwoLevelInvalidationBroadcast(client, channel)`
- `WithTwoLevelInvalidationTimeout(timeout)`
- `WithTwoLevelLogger(logger)`
- `WithTwoLevelSingleFlightForgetTimeout(timeout)`

## 生产建议

推荐：

- L2 统一使用 Redis
- 热点场景优先考虑 `TwoLevelStore`
- L1 需要容量控制时，再显式接入 `Ristretto`
- 响应体较大时一定设置 `WithMaxBodySize`
  超过阈值后会停止继续缓冲并跳过缓存写入
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
- 跨实例 L1 失效广播依赖 Redis Pub/Sub，断线期间的消息不保证补发
- `go test -race ./...` 依赖本机有可用 C 编译器

## License

MIT
