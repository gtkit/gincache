# Changelog

遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与 [Semantic Versioning](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### Added
### Changed
### Deprecated
### Removed
### Fixed
### Security

## [1.3.0] - 2026-08-13

### Added

- 缓存命中时评估条件请求：`GET` / `HEAD` 请求的 `If-None-Match` 与 `If-Modified-Since` 与缓存条目的 `ETag` / `Last-Modified` 匹配时返回 `304 Not Modified`，不再发送 Body。`If-None-Match` 只要存在就压制 `If-Modified-Since`（即使值为空或不匹配），比较采用忽略 `W/` 前缀的弱比较，`*` 匹配任何已缓存的表示。多个 `If-None-Match` 字段行会一并评估，含逗号的合法 opaque-tag（如 `"a,b"`）不会被切开；重复的 `If-Modified-Since` 属畸形请求，整体忽略。
- 缓存命中回放时写出 `Age`，表示该响应自源站生成以来的秒数（RFC 9111 §5.1）。只扣减本级 TTL 不足以保护下游——下游的 CDN 或浏览器看到偏小的 `Age` 会把这份响应再多留一段时间。`ResponseCache` 为此新增 `ResponseTime`（收到响应的时刻）与 `InitialAge`（响应到达时已有的年龄）两个字段。本包此前写入的条目没有它们：既无 `ResponseTime` 也无 `Date` 时年龄无从估算，这类条目按未命中处理并由 handler 重新产生响应回填，每个键只需一次回源且被 singleflight 合并。

### Changed

- **⚠ 破坏性变更**：写入缓存的 TTL 取配置值与响应声明的新鲜期中较小的一个，声明的新鲜期 ≤ 0 时直接拒绝缓存。新鲜期按共享缓存优先级取自 `Cache-Control: s-maxage`、`Cache-Control: max-age`、`Expires` 减 `Date`，并扣除响应已消耗的年龄（`now - Date` 与 `Age` 中较大者），无法解析的 `Expires`（含常见的 `Expires: 0`）按已过期处理。同一新鲜期指令出现多次且取值不同时按陈旧处理（取值相同的重复不算冲突），非法的 delta-seconds 按陈旧处理——`delta-seconds` 的语法是纯数字，`max-age=+600`、`max-age=-1`、单边引号都属非法——超出可表示范围的值钳到最大值。`defaultExpire` 传 0 表示"交由存储决定"，此时中间件不再约束 TTL，只保留"响应已陈旧就不缓存"这道闸门。此前 TTL 完全来自中间件配置，handler 声明 `max-age=60` 而中间件配 10 分钟时会回放 10 分钟，`max-age=0`、`s-maxage=0`、已过期的 `Expires` 也照样被缓存回放。`defaultExpire` 与 `Strategy.CacheDuration` 因此是 TTL 上限而不是最终值。
- **⚠ 破坏性变更**：内置准入基线新增拒绝 `Vary: *` 的响应。按 RFC 9111 §4.1，`Vary: *` 永远匹配失败，该条目不可能被合法复用。具名 `Vary` 维持放行。
- `TwoLevelStore.Get` 区分 L1 二次命中与 L2 回源，分别计入 `local_hit` 与 `remote_hit`。此前 singleflight 内第二次本地检查命中时仍无条件计入远端命中，并发回填期间会把 L1 命中算成 L2 命中，`local_hit_rate` 因此失真。
- **⚠ 破坏性变更**：启用 `persist.WithTwoLevelInvalidationBroadcast` 时必须同时提供 `persist.WithTwoLevelLogger`，否则构造期 panic。订阅启动失败（随机源不可用、Redis 暂时连不上）会让广播在该实例的生命周期内保持关闭，而开启广播正是在依赖跨实例的 L1 收敛——没有 logger 时这个降级毫无信号，故障表现是"多实例偶发读到旧数据"。迁移方式：补上 `WithTwoLevelLogger`。
- **⚠ 破坏性变更**：`persist.TwoLevelStore.Set`（不带 Context 的那个）自带上界，整个操作（远端写入与失效广播）受 `WithWriteTimeout` 约束（默认 3 秒）。此前它用无期限的 Context，而写超时只在 `RedisStore.Set` 内生效，因此这条路径完全没有上界，底层阻塞时会无限等待。需要自己控制期限请用 `SetWithContext`。
- **⚠ 破坏性变更**：`TwoLevelStore` 的写路径改为**失效** L1，不再把新值写进 L1。此前两个并发 `Set` 的远端写顺序与 L1 写顺序可以颠倒，导致 Redis 与 L1 的最终值相反而两个调用都返回成功（实测 Redis 为后写入的值、L1 为先写入的值）。现在并发写都只失效 L1，Redis 留下胜者，下次读从 Redis 回填，两级必然一致。影响：`Set` 成功后本实例该 key 的下一次读会穿到 Redis 并回填，写多读少的 key 上 L1 命中率下降。`SetWithContext` 的返回值也随之只表达 Redis 的写入结果，本地失效失败只记录日志——与 `Delete` 的约定一致。
- **⚠ 破坏性变更**：`persist.WithTwoLevelInvalidationBroadcast` 对 nil 客户端（含 typed-nil）与空 channel 改为构造期 panic，不再静默禁用广播。静默禁用意味着调用方以为开了广播、实际没开，多实例的 L1 会一直陈旧到 `localTTL` 且没有任何信号；typed-nil 此前更会一路走到 `Subscribe` 才 nil 解引用。按条件启用请改用 nil Option（构造函数会跳过它），用法见 README。
- **⚠ 破坏性变更**：以下构造入口改为在构造期拒绝 nil（含装进接口的 typed-nil），不再把 nil 解引用推迟到第一次读写——`persist.NewRedisStore` 的 `client`、`persist.NewTwoLevelStore` 的 `redisClient`、`persist.WithLocalStore` 的 `local`、`ristrettoadapter.New` 的 `cache`、`ristrettoadapter.WithCost` 的成本函数。被拦下的程序原本必然崩在首次读写，迁移方式是传入非 nil 依赖。其中 `ristrettoadapter.New(nil)` 原本**不会崩**：Ristretto 的方法对 nil receiver 安全，它会静默变成黑洞缓存——每次 `Set` 返回 `nil` 装作写成功，每次 `Get` 返回未命中；这类静默失效现在改为显式报错。
- **⚠ 破坏性变更**：`persist.WithLocalTTL(0)` 由静默接受改为在 `NewTwoLevelStore` 构造时 panic。`localTTL` 是 L1 的陈旧上限，取 0 会让本地条目永不过期、也永不被后台清理回收：Redis 条目过期后该实例永久返回旧值，L1 同时失去唯一的回收机制、内存只增不减。迁移方式二选一——想用默认的 30 秒就省略 `WithLocalTTL`，需要别的值就传正数。负数入参的语义不变（忽略，保持默认值）。

### Fixed

- 修复新鲜期计算在极端日期下的整数回绕：`Time.Sub` 超过约 292 年会饱和到 `MinInt64` / `MaxInt64`，两个饱和值相减恰好绕成 `+1ns`，几百年前就过期的响应会被判成新鲜。现在先比较再相减。
- 修复回放 `Age` 时入库年龄与驻留时长相加的溢出：超出可表示范围的 `Age` 被钳到最大值后再相加会回绕成负数，最终输出 `Age: 0`，把极老的响应报成刚出炉。现在改用饱和加法。
- 修复同名不同大小写的 header 被逐个赋值覆盖的问题：它们在 HTTP 语义上是同一个字段，而 Go 的 map 遍历顺序随机，同一份输入会算出不同的新鲜期或不同的条件请求判定结果。现在合并所有大小写变体的值。
- `current_age` 计入 `response_delay`（handler 全程耗时），且与响应是否带 `Age` 无关——此前只在有 `Age` 时才计入，没有 `Age` 的慢响应会拿到完整新鲜期。`Age` 出现列表形式时按 RFC 9110 §5.5 取第一个成员；出现多个字段行时取最大值，而不是整体忽略。
- 修复构造出的 `Strategy.CacheKey` 能伪装成另一类 flight 身份的问题：默认存储此前直接用裸键，自定义存储用"地址+分隔符+键"，两个命名空间之间没有分隔，精心构造的键可以撞进另一个存储的 flight 并拿到别的租户的响应。
- 修复值类型的默认存储完全失去防击穿的问题：靠"取不到地址"判定为自定义存储会给每个请求发独占的 flight 身份，而默认存储自始至终是同一个实例，本该合并。
- 修复 `Expires` 存在而 `Date` 缺失时驻留时间被重复扣减的问题：回放时用当前时间当基准算出的已经是剩余时间，再与含驻留时长的年龄相比，条目在大约一半寿命处就失效。
- 修复旧格式条目回放时忽略条目里 `Age` 的问题：一个上游已经放了 120 秒的响应会被报成 `Age: 0`。
- 接收时刻与初始年龄改为纳秒精度：截成整秒会让初始年龄偏小最多一秒，短新鲜期的条目因此能多活将近一个 `max-age`。
- 修复用接口比较判断存储身份导致的 panic：以值类型实现接口且含切片字段的存储不可比较，比较会直接打崩中间件。身份改用反射取实例地址，拿不到稳定身份的存储获得本次请求独占的 flight 身份。
- 修复 `TwoLevelStore` 的 L1 条目会活过对应 Redis 条目的问题：Redis 命中后的回填此前固定使用完整 `localTTL`，一个只剩 1 秒的 Redis 条目能在 L1 里再活 30 秒，事实来源过期之后本实例仍然返回它。现在回填时长取 `min(localTTL, Redis 剩余 TTL)`；取值与剩余时间在同一次 Redis 往返内取回，往返次数不变。Redis 条目在读取过程中消失时不再回填，本次调用仍会拿到已经读到的值；Redis 条目没有过期时间时使用完整 `localTTL`。
- 修复 L1 条目寿命可能由注入的 `LocalStore` 默认值决定的问题：远端不设过期时间时，本地 TTL 此前被算成 0 并原样传给 `LocalStore`，落到它自己的默认值上。现在 `TwoLevelStore` 始终传出正数 TTL，L1 寿命只由 `localTTL` 与 Redis 剩余 TTL 决定。
- 修复成功的 `Set` / `Delete` / `InvalidateLocal` 会被在飞的读撤销的问题：变更之前发起的那次读在变更完成之后才回填 L1，把已删除的值复活、或把新值覆盖回旧值，本实例因此继续返回旧数据直到 `localTTL` 到期。现在变更会作废该 key 在飞的读，回填前校验读取期间没有发生变更，且校验与写入不可被同 key 的变更插入。收到其他实例的失效广播时同样作废，因此开启广播的多实例部署一并受益。仅解除 singleflight 关联（`Forget`）不足以做到这一点——它不取消已在执行的读取。
- 修复 `persist.RedisStore` 在分片型客户端下按模式删除与统计只覆盖单个节点的问题：go-redis 对无 key 命令（如 `SCAN`）只路由到一个节点，因此 `DeletePattern` 会漏掉其余节点的匹配 key、`Stats` 只统计一个节点（实测 Ring 双 shard 下 64 个 key 只扫到 26 个）。现在 `*redis.ClusterClient` 遍历全部 master、`*redis.Ring` 遍历全部 shard。
- 修复批量删除在 key 跨 hash slot 时失败的问题：一条 `DEL` 带多个 key 时 Redis Cluster 要求所有 key 同属一个 slot，而 go-redis 按首个 key 的 slot 路由且不拆分，跨 slot 会返回 `CROSSSLOT`。现在 `DeleteKeys` 与 `DeletePattern` 都在一次 pipeline 内逐 key 下发，往返次数不变。
- 修复 `persist.MemoryStore` 的过期删除会吃掉并发写入新值的问题：读取到过期条目与删除它之间允许并发 `Set` 换上新值，无条件删除会把刚写入的新值一起删掉（实测 3000 轮中惰性删除丢 112 次、后台清理丢 127 次）。现在只删除自己读到的那个条目。
- 修复可配置 key 前缀被当作 glob 模式的问题：`WithKeyPrefix` 接受任意字符串，而前缀被直接拼进 SCAN 模式，含 `*`、`?`、`[`、`]`、`\` 时会改变匹配范围——实测前缀 `ns*:` 时 `DeletePattern` 会把 `nsother:` 命名空间的 key 一并删掉，`Stats` 也误计。现在只转义固定前缀，调用方传入的 pattern 的通配语义保持不变；单 key 读写删不受影响。默认前缀 `gincache:` 不含元字符，行为不变。
- 修复按模式删除部分成功时不广播失效的问题：删除扇出到多个节点后，"部分分片成功、另一些失败"是可达组合，而广播此前只在完全成功时发出，已删掉的 key 会继续从其他实例的 L1 命中直到本地 TTL 到期。现在只要确实删掉了内容就广播，错误照原样返回；一个都没删掉时不广播。
- 修复批量删除在部分成功时丢弃删除计数的问题：pipeline 的 `Exec` 返回错误不代表所有命令都没执行，此前直接返回零，使 `DeletePattern` 的返回值失真，上层也分不清"部分成功"与"完全失败"。现在累计已成功的删除数，与错误一并返回。
- 修复 `TwoLevelStore.Close` 的幂等承诺对注入实现不成立的问题：`LocalStore` 契约未要求 `Close` 幂等，而此前每次 `Close` 都会再调用一次注入实现的 `Close`。现在注入实现只被关闭一次，重复调用返回首次的结果。
- `persist.RedisStore.Stats` 的 SCAN 批量大小保持为 1000。抽取公共扫描逻辑时它曾被统一成删除路径的 100，使统计的往返次数增至十倍——而它要遍历整个前缀，分片客户端下还要遍历每个节点。两条路径的批量大小现在各自独立。
- 修复缓存写入的分片互斥覆盖了调用方回调的问题：`WithCacheableResponse` 判据此前在临界区内求值，一次慢回调会堵住整个分片的缓存写入，回调若再触发同分片键的缓存写入会直接死锁（互斥不可重入）。现在判据在锁外求值完毕，锁只覆盖"确认未被淘汰 + 写入存储"两步。
- 修复 `TwoLevelStore` 与中间件的淘汰标记在临界区之外判定的问题：两处此前都是"写入前查一次标记"，定时器可以在查过之后触发，新 leader 随即写入更新的结果，而较旧的那份最后落地把它盖掉。`TwoLevelStore` 在无变更时新旧 leader 的代次相同，两个回填都会通过校验。现在标记的检查与它守护的写入处于同一临界区。
- 修复中间件在 singleflight 释放超时之后，较旧的响应会覆盖较新缓存的问题：旧 leader 被释放后新 leader 可能先写入较新的响应，而旧 leader 完成时仍无条件写缓存。现在被释放的 leader 不再写缓存；它仍会把响应返回给自己的调用方，也仍会交给此前已加入的等待者。响应自带的年龄机制兜不住这种覆盖——没有 `Cache-Control` 时新鲜期未声明，回放不会被拒。
- 修复本地变更不作废在飞读取的问题：`Set`、`Delete`、`InvalidateLocal` 之后新发起的读会与变更前开始的那次读合并，从而拿到变更前的值。收到其他实例的失效广播时一直会作废在飞读取，本地变更此前漏了，本地变更的陈旧扩散面因此反而大于远端变更。`DeletePattern` 无法反查在飞读取对应的键，该路径不作废，其窗口受 `localTTL` 约束（见 README 的 L1 陈旧上限）。

### Security

- 升级间接依赖 `github.com/quic-go/quic-go` 到 v0.59.1，关闭 GO-2026-5676（HTTP/3 QPACK Trailer 内存耗尽）。本包代码不涉及 HTTP/3，但 `go.mod` 此前把该模块钉在受影响的 v0.59.0，下游依照最小版本选择会一并拿到。
- L1 失效广播的实例身份改用启动时生成的随机值，不再把实例内存地址写进 Redis Pub/Sub 载荷——凡能订阅该频道的一方此前都能读到这个地址。身份用途不变：接收端据此跳过自己发出的消息。
- **⚠ 破坏性变更**：回放缓存条目前按 `freshness_lifetime > current_age` 判定新鲜度，其中当前年龄计入条目在缓存中的驻留时间。此前只靠存储 TTL 约束时长，`defaultExpire` 传 0（由存储决定）且存储默认值比声明的新鲜期长时，条目会活过自己的新鲜期还被当成新鲜的回放——实测 `max-age=1` 的条目 1.2 秒后仍然命中。
- **⚠ 破坏性变更**：singleflight 把 leader 的响应交给并发等待者之前先判定这份响应可否复用；不可复用时等待者各自执行 handler。此前只在写库路径上拦，等待者照拿不误——实测过三种：`no-store` 请求的响应被原样发给普通请求、超过 `WithMaxBodySize` 被丢弃 Body 的响应回放成空 Body、handler 什么都没写时等待者拿到一个编出来的空 `200`。
- **⚠ 破坏性变更**：singleflight 的合并身份纳入存储身份，且三类身份各带前缀。`Strategy.CacheStore` 允许逐请求选择不同存储（典型用法是按租户分库），此前只按缓存键合并，两个键相同但存储不同的并发请求会共享 leader 的响应——按存储做的隔离被从背后打穿。
- **⚠ 破坏性变更**：遵守请求的 `Cache-Control: no-store`，该请求的响应不再写入缓存（RFC 9111 §5.2.1.5）。已有条目仍可正常命中——这条指令约束的是存储而不是复用，与本包有意不遵守的请求端 `no-cache` 不是同一层级。
- **⚠ 破坏性变更**：`Trailer` 头列出的字段、以 `http.TrailerPrefix` 为前缀的运行期 trailer 键，以及 `Proxy-Authentication-Info` 不再进入缓存。此前只删除了 `Trailer` 声明本身，它指向的字段会被当成普通响应头缓存并回放。
- **⚠ 破坏性变更**：`New` 拒绝装进接口的 typed-nil 存储；`Strategy.CacheStore` 为 typed-nil 时跳过缓存而不是退回默认存储——按存储做租户隔离时，退回默认存储等于把请求写进别人的库。
- **⚠ 破坏性变更**：`DefaultCacheableResponse` 与内置中间件对请求头的判断全部改为大小写无关，覆盖 `Authorization`、`Range`、`If-None-Match` 与 `If-Modified-Since`。此前只按规范键查找，`http.Header{"set-cookie": {"x=1"}}` 会被判定为可缓存，请求头里直接写入的小写 `authorization` 也能绕过门禁并让多个请求共享同一份缓存，小写 `range` 更会让范围请求命中缓存、把完整响应当成范围响应发出。真实网络流量经 net/http 解析后键必为规范形式，但程序化构造的请求、前置中间件与直接写 map 的 handler 都会踩到。

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
