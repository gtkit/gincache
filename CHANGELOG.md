# Changelog

遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与 [Semantic Versioning](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### Added

- 缓存命中时评估条件请求：`GET` / `HEAD` 请求的 `If-None-Match` 与 `If-Modified-Since` 与缓存条目的 `ETag` / `Last-Modified` 匹配时返回 `304 Not Modified`，不再发送 Body。`If-None-Match` 优先于 `If-Modified-Since`，比较采用忽略 `W/` 前缀的弱比较，`*` 匹配任何已缓存的表示。多个 `If-None-Match` 字段行会一并评估，含逗号的合法 opaque-tag（如 `"a,b"`）不会被切开。

### Changed

- **⚠ 破坏性变更**：写入缓存的 TTL 取配置值与响应声明的新鲜期中较小的一个，声明的新鲜期 ≤ 0 时直接拒绝缓存。新鲜期按共享缓存优先级取自 `Cache-Control: s-maxage`、`Cache-Control: max-age`、`Expires` 减 `Date`，并扣除响应已消耗的年龄（`now - Date` 与 `Age` 中较大者），无法解析的 `Expires`（含常见的 `Expires: 0`）按已过期处理。同一新鲜期指令出现多次时取最小值，非法的 delta-seconds（非数字、负数、单边引号）按陈旧处理，超出可表示范围的值钳到最大值。此前 TTL 完全来自中间件配置，handler 声明 `max-age=60` 而中间件配 10 分钟时会回放 10 分钟，`max-age=0`、`s-maxage=0`、已过期的 `Expires` 也照样被缓存回放。`defaultExpire` 与 `Strategy.CacheDuration` 因此是 TTL 上限而不是最终值。
- **⚠ 破坏性变更**：内置准入基线新增拒绝 `Vary: *` 的响应。按 RFC 9111 §4.1，`Vary: *` 永远匹配失败，该条目不可能被合法复用。具名 `Vary` 维持放行。
- `TwoLevelStore.Get` 区分 L1 二次命中与 L2 回源，分别计入 `local_hit` 与 `remote_hit`。此前 singleflight 内第二次本地检查命中时仍无条件计入远端命中，并发回填期间会把 L1 命中算成 L2 命中，`local_hit_rate` 因此失真。

### Deprecated
### Removed
### Fixed
### Security

- **⚠ 破坏性变更**：`DefaultCacheableResponse` 与内置中间件的 `Authorization` 门禁改为大小写无关地读取 header。此前只按规范键查找，`http.Header{"set-cookie": {"x=1"}}` 会被判定为可缓存，请求头里直接写入的小写 `authorization` 也能绕过门禁并让多个请求共享同一份缓存。真实网络流量经 net/http 解析后键必为规范形式，但程序化构造的请求与直接写 map 的 handler 都会踩到。

## [1.2.0] - 2026-08-10

本版本包含多项 **⚠ 破坏性变更**，全部为 fail-closed 形态——只会让更少的响应进入缓存，
不会改变已被判定可缓存的响应内容。升级后无需清空 store 或轮换 key 前缀：缓存键格式变化
会让旧条目自然落空，命中回放前的复检会挡下不合规的历史条目。

### Added

- 新增 `WithCacheableResponse` 与 `CacheableResponse`，用于判定一个已经产生的响应能否被共享缓存；请求期策略跑在 handler 之前，看不到响应头，无法表达这类约束。
- 新增 `DefaultCacheableResponse`，即未设置 `WithCacheableResponse` 时生效的内置准入基线，可在自定义判据中显式组合。

### Changed

- **⚠ 破坏性变更**：默认可缓存状态码集合排除 `206 Partial Content`，且携带 `Range` 头的请求整体绕过缓存（既不读也不写）。缓存键不含 `Range`，此前不同范围的请求会互相拿到对方的字节片段。需要缓存 `206` 的调用方可显式配置 `WithCacheableStatusCodes(206)`。
- **⚠ 破坏性变更**：Handler 调用 `Hijack` 接管连接（WebSocket 升级等）或未写出任何内容时不再产生缓存条目。此前会缓存成一个空 `200`，顶掉后续所有请求。
- **⚠ 破坏性变更**：连接级 header 不再进入缓存，命中回放时也不再写出——`Connection`、`Proxy-Connection`、`Keep-Alive`、`TE`、`Trailer`、`Transfer-Encoding`、`Upgrade`、`Proxy-Authenticate`、`Proxy-Authorization`，以及 `Connection` 头值中列出的字段名。
- **⚠ 破坏性变更**：缓存命中回放前会用当前判据复检条目的状态码与响应头，不通过则视为未命中执行 handler。因此升级本身就能挡住此前写入的不合规条目，无需清空 store 或轮换 key 前缀。
- **⚠ 破坏性变更**：`CacheByRequestURI` 与 `CacheByRequestPath` 的缓存键纳入 HTTP method，键格式变为 `"<method> <uri>"`，`GET` 与 `HEAD` 各用各的键。此前挂在 `r.Any()` 或混方法路由组下时，`POST` 会复用 `GET` 的响应；而把 `HEAD` 归一到 `GET` 键上会让"HEAD 分支不产出 Body"的普通 handler 在 HEAD 先到时把空条目写进 `GET` 键。升级后旧缓存条目会落空一次，属于一次冷启动。
- **⚠ 破坏性变更**：`CacheByRequestURI` 与 `CacheByRequestPath` 只缓存 `GET` 与 `HEAD`，其余方法直接放行。此前 `POST` / `PUT` / `DELETE` 的响应会被缓存并回放，让后续同键请求跳过业务处理——副作用不会发生而调用方收到成功响应。需要缓存这些方法的调用方改用 `Cache` 配合 `WithCacheStrategyByRequest`。
- **⚠ 破坏性变更**：`New` 在 store 为 nil 或默认 TTL 为负数时于构造期 panic 并给出明确信息，`opts` 中的 nil 元素被跳过。此前 nil store 要等第一个请求进来才以 nil 解引用的形式暴露。
- `WithMaxBodySize` 与 `WithSingleFlightForgetTimeout` 忽略负数入参，保持各自"不限制"与"不设定时器"的默认语义。
- `NewTwoLevelStore` 在 client 为 nil 时于构造期 panic 并给出明确信息，`opts` 中的 nil 元素被跳过；`WithLocalTTL` 与 `WithRemoteTTL` 忽略负数入参。此前 nil client 要等第一次读写才以 nil 解引用的形式暴露。

### Fixed

- 修复 singleflight 释放定时器从不停止的问题：定时器此前在每个请求上无条件创建且从不 `Stop`，已结束请求的定时器会在后续同 key 请求执行期间调用 `Forget`，导致同一个 key 出现多个 leader、防击穿失效，高 miss 流量下还会持续积累无效定时器。
- 修复 `TwoLevelStore` 在 Redis 写入成功、L1 写入失败时不清理本地旧值的问题：本实例此前会继续命中过期数据直到本地 TTL 到期，现在会主动失效该 key 的 L1 条目并返回带上下文的错误。
- `TwoLevelStore.Delete` 与 `DeletePattern` 的本地删除失败不再被静默丢弃，改为经 `WithTwoLevelLogger` 记录；两者的返回值仍然只表达 Redis 的结果。

### Security

- **⚠ 破坏性变更**：默认拒绝缓存携带 `Set-Cookie`、`Cache-Control: no-store`、`Cache-Control: private` 或 `Cache-Control: no-cache` 的响应。`no-cache` 的语义是"未经重新验证不得复用"，而本包没有 revalidation 机制，存了就必然无验证复用。此前这类响应会被缓存并回放给后续所有请求，等于把第一个用户的会话发给别人。需要恢复旧行为的调用方可传入恒为真的判据：`WithCacheableResponse(func(int, http.Header) bool { return true })`。
- **⚠ 破坏性变更**：singleflight 把 leader 的响应交给并发等待者之前同样复检。这条路径绕过 store，此前完全不受任何判据保护，leader 的 `Set-Cookie` 会被原样发给所有同 key 的并发等待者。复检不通过时等待者改为执行自己的 handler。
- **⚠ 破坏性变更**：`CacheByRequestURI` 与 `CacheByRequestPath` 对携带 `Authorization` 的请求整体绕过缓存。内置键不含任何请求头，无从区分不同凭据的用户（RFC 9111 §3.5）。需要缓存这类响应的调用方改用 `Cache` 配合 `WithCacheStrategyByRequest` 自行拼键。
- **⚠ 破坏性变更**：响应头的键在写入缓存与命中回放前统一规范化。此前 handler 通过直接写 map（而非 `Header.Set`）留下的 `set-cookie`、`connection` 这类非规范键能绕过准入基线与逐跳 header 过滤，让 `Set-Cookie` 完整地进缓存再发出去。

## [1.1.0] - 2026-05-28

### Added

- 新增 `TwoLevelStore` 基于 Redis Pub/Sub 的跨实例 L1 失效广播，支持 `Set`、`Delete`、`DeletePattern` 后清理其他实例的本地缓存。
- 新增 `WithTwoLevelLogger`、`WithTwoLevelInvalidationTimeout` 和 `WithTwoLevelSingleFlightForgetTimeout`，用于广播链路可观测性、订阅启动超时和慢回源释放。
- 新增 README 中 BigCache、FreeCache 等本地缓存库通过 `persist.LocalStore` 接入的说明和 BigCache 适配器骨架。

### Changed

- JSON 编解码统一使用 `github.com/gtkit/json`，并通过回归测试禁止项目源码直接导入 `encoding/json`。
- `persist/ristrettoadapter` 子模块显式依赖 `github.com/gtkit/json`，保证 `go mod tidy` 后依赖不会被误删。

### Fixed

- 修复示例中 `Close` 返回错误未处理的问题。
