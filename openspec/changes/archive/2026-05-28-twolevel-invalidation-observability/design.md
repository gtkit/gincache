## Context

TwoLevelStore 使用本地 L1 和 Redis L2。单实例或弱一致业务可以只依赖 L1 TTL，但多实例场景下，某实例更新或删除 Redis 后，其他实例的 L1 仍可能持有旧值直到 TTL 到期。

本次变更需要在不引入复杂分布式一致性协议的前提下，将常见失效窗口从默认 30 秒级压缩到 Redis Pub/Sub 可达时的亚秒级，并让广播链路故障可排查。

## Goals / Non-Goals

**Goals:**

- 支持多实例 TwoLevelStore 通过 Redis Pub/Sub 清理其他实例的 L1。
- 对发布失败、订阅启动失败、订阅异常关闭、脏消息、本地删除失败提供日志。
- 广播客户端能力通过类型签名表达，避免运行期静默关闭功能。
- Redis 不可达时，订阅启动必须受默认 5 秒超时约束，并降级为无广播模式。
- singleflight 等待必须有默认超时释放能力。

**Non-Goals:**

- 不实现强一致缓存协议。
- 不保证 Redis Pub/Sub 断线期间消息补发。
- 不引入布隆过滤器或空值缓存穿透防护。
- 不把发布改成异步 fire-and-forget。

## Decisions

- `NewTwoLevelStore` 继续接收 `redis.Cmdable`，保持未开启广播时的客户端兼容性。
- `WithTwoLevelInvalidationBroadcast` 接收 `redis.UniversalClient` 和 channel，使 Pub/Sub 能力在开启广播时由编译期保证。
- 每个 TwoLevelStore 实例生成内部 origin，订阅端忽略本实例发布的消息，避免刚写入的 L1 被自己的广播删除。
- 订阅启动使用 `context.WithTimeout` 和 `PubSub.Receive` 等待订阅确认；失败时关闭 pubsub、记录日志并禁用广播。
- 监听 goroutine 捕获 panic 并记录日志，`Close` 主动关闭时不记录异常关闭日志。
- 发布保持同步执行，减少失效消息排队的不确定性；调用方需要接受 Redis Publish 耗时计入写路径延迟。

## Risks / Trade-offs

- Pub/Sub 不是可靠消息队列，断线期间的失效消息可能丢失；Redis L2 仍是事实来源，L1 TTL 仍是最终兜底。
- 同步发布会拉长 `Set`、`Delete`、`DeletePattern` 的尾延迟，但实现更简单，失败也更容易观测。
- 广播依赖 `redis.UniversalClient`，调用方如果只持有较窄的 `redis.Cmdable` 变量，需要在开启广播处传入具体 Redis 客户端或拓宽变量类型。
