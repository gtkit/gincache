## Why

TwoLevelStore 在多实例部署中需要让 L1 本地缓存更快收敛，并且失效广播链路不能在发布、订阅、消息解析或本地清理失败时静默退化。

当前业务对订单状态、价格、库存、路由响应这类数据的失效敏感度较高，默认等待 L1 TTL 自然过期会产生过长的不一致窗口。

## What Changes

- 新增基于 Redis Pub/Sub 的 TwoLevelStore L1 失效广播，`Set`、`Delete`、`DeletePattern` 成功后发布失效消息。
- 新增 `WithTwoLevelInvalidationBroadcast`，显式接收 `redis.UniversalClient`，用编译期契约表达 `Publish` 和 `Subscribe` 能力。
- 新增 TwoLevelStore 后台错误日志接口，用于记录订阅启动失败、订阅异常关闭、消息解析失败、本地失效失败和发布失败。
- 新增订阅启动确认超时，避免 Redis 不可达时阻塞构造流程。
- 为 TwoLevelStore 内部 singleflight 增加超时 `Forget`，避免同 key 慢回源长期阻塞后续请求。

## Capabilities

### New Capabilities

- `twolevel-cache-coherence`: TwoLevelStore 的跨实例 L1 失效广播、后台可观测性、订阅启动超时和 singleflight 超时释放能力。

### Modified Capabilities

无。

## Impact

- 影响 `persist.TwoLevelStore` 的 Option API、Set/Delete/DeletePattern/Get/Close 行为和后台 goroutine 生命周期。
- 依赖 Redis Pub/Sub；广播客户端必须实现 `redis.UniversalClient`。
- README 和 example 需要同步展示广播、日志、超时和 singleflight 配置。
