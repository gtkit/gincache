# bigcacheadapter

独立可选 Go module：`github.com/gtkit/gincache/persist/bigcacheadapter`。只引入 BigCache v3.2.0，不捆绑另外两种原生引擎；仍继承主模块已有的 Gin、Redis、JSON 依赖。

本模块版本为 v0.1.0，依赖主模块 v1.4.0。下方完整示例已在发布检查中构建，并运行独立和共享两种模式。

## 按需引入

以下只选择 BigCache。示例目录为同级的 `cache-demo/` 与 `gincache/`；如果本机目录不同，将 `replace` 的右侧改为实际路径。这里只展示调用方的开发配置，不修改本仓正式 go.mod。

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
	github.com/gtkit/gincache v1.4.0
	github.com/gtkit/gincache/persist/bigcacheadapter v0.1.0
)

replace github.com/gtkit/gincache v1.4.0 => ../gincache

replace github.com/gtkit/gincache/persist/bigcacheadapter v0.1.0 => ../gincache/persist/bigcacheadapter
```

上面的替换仅用于本地源码联调。使用发布版本时移除这些本地替换，再按需安装：

```bash
# 只安装所选模块，不需要安装另外两个适配器。
go get github.com/gtkit/gincache/persist/bigcacheadapter@v0.1.0
```

## 完整示例：独立实例与共享实例

将下面保存为演示项目的 `main.go`。这是单文件示例，只导入 BigCache 适配器；不需要同时复制其他引擎的示例。`-shared=false` 创建两个原生实例，`-shared=true` 让两个 namespace 共用一个实例。

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

	"github.com/allegro/bigcache/v3"
	"github.com/gin-gonic/gin"
	"github.com/gtkit/gincache"
	"github.com/gtkit/gincache/persist"
	"github.com/gtkit/gincache/persist/bigcacheadapter"
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

在演示项目目录执行以下命令：

```bash
GOWORK=off go mod tidy

# 两个 Owner、两个原生实例，业务和 HTTP 各自一份预算。
GOWORK=off go run . -shared=false

# 一个 Owner、一个原生实例，两个 namespace 共用预算。
GOWORK=off go run . -shared=true
```

示例通过两次同步内存 HTTP 请求演示 gincache，不监听端口；请求全部结束后才关闭资源。正常命中时 `handler_calls=1`，但缓存准入和淘汰允许 miss，不应把命中次数当成业务保证。`readText` 遇到 miss 只打印演示标记；真实业务应向数据源回源。发布检查已构建本示例，并运行独立和共享两种模式。

## 独立与共享、键隔离

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

## TTL 与错误语义

| 写入 TTL | 含义 |
|---|---|
| 正数 | 本次条目的逻辑 TTL，从写入调用入口计时，包含编码和等待消耗 |
| `0` | 回落到该视图的 `WithStoreExpiration`；未设置时默认 `0`，无逻辑期限 |
| 负数 | 返回 `ErrInvalidArgument` |

命中不续期；到达精确期限返回 `persist.ErrCacheMiss`，即使原生条目尚未回收。原生更早淘汰允许 miss，不能延长逻辑有效期。无逻辑期限不等于永不淘汰。

`SetBytes` 返回 `(stored, err)`，正常准入或容量拒绝可以是 `(false, nil)`。`Set` / `SetWithContext` 遇到这种拒绝也返回 nil，因此 nil 只表示没有真实写入错误，不保证下一次 Get 一定命中。`Get` / `Set` 使用 JSON 编解码，字节接口用于无需该层 JSON 编码的值。

## 生命周期与借用已有实例

`NewOwner` 创建并拥有原生实例；`WrapOwner` 只借用。每个原生对象只能包装成一个 Owner，再由该 Owner 分配多个 namespace，不能为了共享把同一裸对象分别 Wrap 两次。

1. 进程装配阶段创建 Owner 和视图；不要每个请求重新创建。
2. 运行中经视图读写，不绕过 Owner 直接写入、清空或关闭原生实例。
3. 关停时先停止新请求和后台生产者，等待所有缓存消费者退出。
4. `Store.Close()` 只关闭该视图，不关闭共享 Owner；创建方最后调用 `Owner.Close(ctx)`。
5. 使用 `WrapOwner` 时，Owner 关闭后原生对象仍由原创建方管理；全部使用者退出后按原生库的方式释放资源。

Owner 等待操作门时响应 context；已进入的原生同步调用、Wait 或 Close 不能被 context 强制打断。Close 超时不代表原生操作已经停止，不能据此提前关闭借用对象或宣称消费者已经退出。

## BigCache 容量与原生约束

- `HardMaxCacheSize` 必须为正，单位 MB；示例每个 Owner 配置 16 MB。
- `Shards` 必须为正且为 2 的幂；容量、索引、临时编码对象与进程 RSS 不是同一概念。
- `LifeWindow` 是原生过期/回收参数，不替代每条记录的逻辑 TTL。原生提前清理允许造成 miss，不能延长逻辑期限。
- 大条目无法写入时保留原生错误；不要只按原始 HTTP body 大小估算编码后记录。

借用已有对象的入口：`bigcacheadapter.WrapOwner(raw, originalConfig)`。原始配置/容量必须来自真实构造参数；这里返回的 Owner 不拥有裸对象，不会替原创建方关闭它。示例代码使用 NewOwner，因此由示例负责最终 Close。

## gin_api 当前接入限制

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

## 相关说明

- [主 README 的通用接入与三种驱动示例](../../README.md#local-adapter-examples)
- [主 README 的 gin_api 接入限制](../../README.md#gin-api-cache-integration)
- [已有 Owner Example](example_owner_test.go)
