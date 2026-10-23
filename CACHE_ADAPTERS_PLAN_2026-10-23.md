> 2026-10-23 后续决定：本方案已被用户要求整体撤回；历史实施记录保留，不代表当前 gin_api 状态。后续以 CACHE_ADAPTERS_MINIMAL_PLAN_2026-10-23.md 及 cache-adapters-rollback-plan-2026-10-23.md 为准。

# 通用缓存接口与 gin_api 调用方适配方案

修订日期：2026-10-23，Asia/Taipei。
状态：S1–S8 已完成，获准的五包检查和最终 OpenSpec 严格校验已通过，当前变更已归档；未提交或发布。文末记录本轮真实命令与边界，保留设计阶段历史。
主项目：/Users/xiaozhaofu/go/src/my-gtkit-package/gincache。
调用项目：/Users/xiaozhaofu/go/src/Project/gin_api。

配套文档：[gin_api 适配与实例装配实施方案](../../Project/gin_api/docs/cache-sharing-adapters-plan-2026-10-23.md)。本文件固定跨项目职责、共享契约、阶段编号和验收；配套文档固定 gin_api 的接口及文件范围。两份文档须一起解释。

## 1. 需求来源与本次边界

最新用户要求：

> gincache 只依赖通用接口，BigCache、FreeCache、Ristretto 的适配放在 gin_api，由调用方选择共享或独立实例，明确键隔离、TTL、容量和生命周期，暂不修改代码。

本修订取代上一版“在 gincache 新增 BigCache、FreeCache 独立 module”的决定。针对目前由 gin_api 使用的场景，采用使用方持有接口、调用方承担具体适配与装配的方案；这不是对所有项目的绝对最优或性能最优承诺。

本次只更新本文件与配套文档，不修改 Go 文件、go.mod/go.sum、配置、README、既有基础计划、错误日志或 OpenSpec；不运行测试、build、vet、lint、modern、race、覆盖率、benchmark、服务或容器命令；不提交、打标签或发布。下文新增符号、文件、配置说明和用例都是待实施设计。

## 2. 现状与兼容约束

本次根据实际源码再次核对：

| 位置 | 已确认事实及约束 |
|---|---|
| gincache/persist/store.go | CacheStore 已有 Get、Set、Delete、SetWithContext；无需再造同义接口 |
| gincache/persist/local_store.go | LocalStore 已供 TwoLevelStore 注入，另含 Close、Stats、ResetStats；模式删除为可选扩展 |
| gincache/persist/twolevel.go | 保持 L1 → Redis → 回源；写入 Redis 后失效 L1；L1 回填受 Redis 剩余 TTL 限制 |
| gincache/go.mod | Go 1.27；没有三种原生引擎的直接依赖；现有 Redis、MemoryStore 不属于本次移除范围 |
| gincache/persist/ristrettoadapter | 现有独立 module，原生依赖 v2.4.2；其独立发布和现有用户必须保持兼容 |
| gin_api/go.mod | Go 1.27.3；gincache v1.3.1、旧 ristrettoadapter v1.1.0、原生 Ristretto v2.4.2、gtkit/json/v2 v2.1.1 |
| gin_api/internal/pkg/cache/cache.go | 当前仍是泛型 Cache[T] 加 map/LRU；尚无 Backend、NewWithBackend、Drain |
| gin_api/internal/pkg/httpcache/cache.go | 默认 memory 分支创建 Ristretto 并使用旧适配模块；另有 Redis 分支及既有 HTTP 准入策略 |
| gin_api/internal/platform/responsecache/owner.go | 已有 Options.Factory；拥有 HTTP 包装对象，不应据此拥有外部共享引擎 |
| gin_api/internal/bootstrap | 公开 Options 尚无 ResponseCacheFactory；资源装配和关闭顺序已有实现 |
| gin_api/docs/cache-adapter-design-plan-2026-10-23.md | 另一基础方案已有 P1–P4 授权记录；授权记录不代表代码完成，本轮不接手、不覆盖 |

缺失输入：生产 QPS、值大小分布、命中率、RSS 预算、实际业务 T/codec、关停预算和性能基线。设计可先固定；生产选择与启用须补齐这些输入，不能编造容量、吞吐或兼容性结果。

## 3. 目标、非目标和取舍

目标：

1. gincache 对三种可选本地引擎只认现有接口，不增加原生依赖或新适配 module。
2. gin_api 提供三种本地适配，共用必要的键隔离、编码、统计和生命周期逻辑。
3. 业务 Cache[T] 保留治理逻辑和默认 LRU，通过自身小接口注入；HTTP 通过 CacheStore 注入。
4. 同一进程的调用方可选择一个底层实例共享，或多个实例独立；两种方式使用同一套消费 API。
5. 不改变现有公开函数签名、默认启用状态、Redis 路径、HTTP 准入、安全与响应协议。

非目标：动态插件发现、全局注册表、字符串反射选引擎、热切换、跨进程共享本地对象、跨缓存事务、命名空间硬配额、自动迁移业务模块，以及顺便修复既有适配模块的所有问题。

第一版仅实现必需的 CacheStore、LocalStore 和业务字节接口，不要求新适配器实现可选 DeletePattern。单键失效已覆盖本次需求；不为枚举删除增加通配符协议、索引或全局清空能力。既有 gincache 及旧适配模块的模式删除功能不删除。

| 候选 | 依赖和维护 | 兼容、成本与回滚 | 决定 |
|---|---|---|---|
| gincache 主模块直接引入三个引擎 | 所有主库使用者承担原生依赖和维护 | 扩大依赖面，与最新要求不符 | 不采用 |
| gincache 新增可选适配子模块 | 核心仍轻，但需要多个模块的发布、版本和契约维护 | 适合已有跨项目复用需求；目前没有该证据 | 取代上一版决定 |
| gin_api 内集中适配，gincache 沿用接口 | 具体依赖只在调用方，公共层不认识引擎 | 不需发布新 gincache 版本；可回默认构造路径 | 采用 |

这里的“接口”是 Go 的 interface。原生库签名不同，且不能在别的包给其类型增加方法，因此由 gin_api 的包装类型实现；不需要修改三个上游库，也不需要泛型插件框架。

## 4. 职责、目录和依赖方向

    gin_api 的调用方／现有装配层
      ├─ 一个 Owner，共享底层实例
      │    ├─ 业务 namespace → ByteStore → Backend[T] → Cache[T]
      │    └─ HTTP namespace → persist.CacheStore → gincache
      └─ 两个 Owner，独立底层实例和预算
           ├─ 业务 Owner → 业务 namespace
           └─ HTTP Owner → HTTP namespace

    gin_api/internal/pkg/cacheadapter
      ├─ BigCache 原生转换
      ├─ FreeCache 原生转换
      └─ Ristretto 原生转换
          共用 Owner、Store、记录格式；私有引擎接口不对外发布

    gincache → 自己的 CacheStore / LocalStore
    gin_api/cacheadapter → gincache/persist + 选定原生库
    gin_api/cache → 自己的 Backend[T] / ByteStore / Codec[T]

新增 internal/pkg/cacheadapter 是同层、无业务语义的基础设施适配包，供 HTTP 和业务装配共用；不搬动任何现有包，不改变业务层依赖方向。application 不直接 import 具体引擎或此基础设施适配包。

### 4.1 既有 Ristretto 适配模块的处理

- gincache/persist/ristrettoadapter 保留原位置、接口和独立 module；本次不删除、不重命名、不搬移源码。
- gin_api 新增自己的适配实现，不复制旧实现中的键跟踪或未处理 context 行为作为新契约。
- gin_api 默认 memory 路径在后续实施中迁到本地 Ristretto 适配，仍选择 Ristretto；确认全项目不再引用旧 module 后，才移除 gin_api 对其直接依赖。
- 旧模块继续兼容其它消费者。任何弃用或停止发布需单独决策，不属于本计划。
- gincache 不新增 BigCache/FreeCache 目录，不改 Makefile 的子模块列表，不为本任务发布新版本。

## 5. 共享与独立实例的固定语义

| 项目 | 共享实例 | 独立实例 |
|---|---|---|
| 原生对象 | 一个 Owner 管一个原生对象 | 每个 Owner 管各自对象，可选不同引擎 |
| 键 | 每个消费者独立 namespace | 仍使用 namespace，避免迁移时重新定义键 |
| 容量和淘汰 | 共用总预算，允许互相挤出条目 | 分别配置预算和淘汰范围 |
| TTL、codec、统计 | 各 Store 独立配置和计数 | 各 Store 独立配置和计数 |
| 最终释放 | 所有消费者退出后释放一次 | 各实例使用者退出后各释放一次 |

同一个原生指针只能建立一个受管 Owner；多个消费者拿这个 Owner 的不同 Store。不能分别 Wrap 同一裸对象，也不能旁路调用其 Set/Del/Clear/Reset/Close。共享只在同一进程内成立，不意味着业务记录与 HTTP 响应互通，也不自动完成两类失效。

优先选择依据：想统一总预算、能接受相互淘汰时用共享；需要容量和生命周期边界时用独立。没有生产负载证据，不把共享宣称为必然更省内存，也不替调用方默认开启共享。

## 6. 键、TTL 与存储格式

### 6.1 键隔离

物理键固定为：

    "gac2:" + 十进制(namespace 的 UTF-8 字节数) + ":" + namespace + ":" + logicalKey

namespace 必填，合法 UTF-8、无 NUL、1–128 字节，不裁剪或大小写转换；logicalKey 原样保留。示例为 api.biz.article.v1、api.http.v1。业务类型、schema 或租户需要隔离时由装配者选择不同 namespace；不按每次请求创建 Store。

相同逻辑键在不同 Store 中不得串读、覆盖或互删。Store.Close、ResetStats 不清引擎或别的 Store；不提供全局 Clear/Reset。命名空间是可信进程内的逻辑隔离，不是安全沙箱。

### 6.2 TTL

- expire > 0：按写入入口捕获的 now 计算逻辑期限；expire == 0：使用 Store 默认值，默认值 0 表示无逻辑期限；负值拒绝。
- 读取时 now >= expiresAt 即 miss；命中不续期，过期读取不无条件删键，避免误删并发更新。
- 业务 Cache[T] 只计算一次带抖动的 ExpiresAt，业务记录保留绝对期限，读取再次判断。存储层只能提前 miss，不能延长该期限。
- HTTP 沿用现有路由 TTL 与响应新鲜度规则；TwoLevelStore 注入路径不提升其给出的短 TTL，L1 不超过 Redis 剩余有效期。
- BigCache 的全局 LifeWindow/CleanWindow、FreeCache 的整秒期限和 Ristretto 的原生过期均只协助回收；正确性以记录的逻辑期限为准。
- FreeCache 正期限辅助回收用 ceil(ttl/1s)+1s；转换或 uint32 到期范围不安全时回落原生无期限，逻辑检查仍有效。绝不把亚秒 TTL 截断后误当成永不过期。
- SetWithContext 在编码前、等待操作权、进入原生写入前后检查 ctx。原生同步调用与 Ristretto.Wait 没有可中断 API；不能声称已进入的调用会在 deadline 精确中止，也不新增每次调用一个 goroutine 来伪装取消。要求硬中断的场景不属于这个进程内接口保证。

### 6.3 统一记录

适配包内部记录采用 GAC2：4 字节 magic、1 字节期限标志、8 字节有符号 Unix 秒、4 字节纳秒、8 字节写入序号、4 字节物理键字节数、完整物理键、payload；定长头为 29 字节。整数为大端；期限标志只允许 0/1，无期限时秒和纳秒为 0，纳秒必须小于 1e9；长度字段须在实际缓冲区内，序号非零且溢出时报错。

保留完整物理键，用于原生哈希冲突校验及 Ristretto 的 IterValues 命名空间统计，避免无界的附加键索引。序号用于确认本次写入，而非把旧值误认为本次准入成功。此格式是新设计，尚未写入缓存；上一版 17 字节方案同样未实施，不需要运行期迁移。

短记录、未知版本、非法字段报可识别损坏错误；物理键不匹配视为 miss，不返回其它键的 payload。读写字节复制，调用方修改 slice 不污染存储。字节路径不重复 JSON 编码；HTTP any 路径使用 gin_api 已有 gtkit/json/v2。

## 7. 容量、错误和观察口径

| 引擎 | 调用方提供的预算 | 不得混淆的边界 |
|---|---|---|
| BigCache v3.2.0 | 正 HardMaxCacheSize，原生单位 MB | 主要限制队列；索引、临时对象不在此上限内；预分配提示不是单条硬限制 |
| FreeCache v1.2.7 | 字节预算，至少 512 KiB | 原生会抬高过小预算，新构造直接拒绝，避免静默超出声明 |
| Ristretto v2.4.2 | 正 MaxCost、NumCounters、BufferItems | MaxCost 是成本单位；本适配固定按物理键及完整记录字节计费，原生内部成本另计，不是 RSS |

共享模式只有一份 B_shared；独立模式分别有 B_biz 与 B_http。进程估算为引擎队列/成本对应对象 + 索引/计数器 + 记录副本 + codec 临时对象 + 在途结果 + 其它服务内存；没有实测时不能把任一预算标为 RSS 硬限额。

FreeCache 单条还须满足锁定版本的限制：physicalKeyBytes <= 65535，physicalKeyBytes + storedValueBytes <= floor(floor(capacityBytes / 256) / 4) - 24。storedValueBytes 现在包括 29 字节头和另一份完整物理键。

例如 64 MiB 缓存的键值合计上限为 65512 字节；HTTP 默认 1 MiB body 仅 base64 就是 4 × ceil(1048576 / 3) = 1398104 字节，尚无头和记录元数据。不能把“body 允许缓存”理解为“FreeCache 能容纳”。调用方可缩小响应准入、为 HTTP 选其它引擎或独立实例，不自动扩大生产内存。

统一语义：

- 正常 miss 映射 persist.ErrCacheMiss；业务字节桥接以显式注入的 miss 哨兵判断，业务基础包不 import persist。
- 字节 Set 的 stored=false、err=nil 表示已识别的容量/准入拒绝，loader 与 HTTP 响应保持成功，不自旋重试。
- Ristretto 的 Set=true 只是原生接收；处理屏障后再核对完整键和写入序号。Wait 不能保证策略准入或永久保留。
- FreeCache ErrLargeEntry 是正常拒绝，ErrLargeKey 是输入错误；BigCache 原生写错保留错误，不按字符串猜测为成功或拒绝。
- CacheStore.Set/SetWithContext 对正常拒绝返回 nil；这是一次缓存尝试完成，不是未来必命中的承诺。
- 真正存储/codec/损坏错误可区分，不能输出值、完整 key 或凭据；业务故障按基础方案进入可观测 bypass。
- Stats 中操作计数只属于 Store；keys/bytes 等盘点来自原生枚举，是 O(N) 快照，不能高频当廉价监控。Ristretto 原生枚举可跳过已原生过期项，不声称三种引擎的物理条目数完全相同。
- 原生总占用、淘汰与 RSS 不能作为 namespace 独占统计。无依据的指标不伪造。

## 8. 并发与生命周期

Owner 统一控制同一原生对象的操作、视图登记和关闭。第一版沿用单个、可通过 ctx 取消等待的操作门，串行化原生读写、枚举与关闭；不宣称保留原生多线程吞吐。编码/解码放在门外，进入门后复检视图状态、ctx 和逻辑期限。若这项成本不符合实测预算，停止推广并单独评审并行化，不能未经证明拆掉顺序保护。

Ristretto 写入返回前完成缓冲屏障与本次序号确认；Delete 返回前完成删除屏障，防止旧排队写入复活。只读枚举回调不调用原生写操作，避免重入原生锁。用户回调不得阻塞或反向调用 Owner/Cache；新 Ristretto 构造不开放自定义准入/回收回调，Wrap 必须由创建方确认无此旁路。

锁顺序固定为业务 Cache 的既有状态顺序 → Owner 操作门，不允许反向调用。原生清扫/淘汰可提前移除条目，不能产生跨键命中。

| 对象 | 拟定生命周期 |
|---|---|
| New* 创建的 Owner | 明确拥有原生对象；构造可启动原生工作协程，禁止 init/包级隐式创建；最终释放一次 |
| Wrap* 创建的 Owner | 借用原生对象，只关闭本身和视图；原创建者最终释放 |
| Store.Close() | 幂等关闭当前视图，不清其它视图，不释放 Owner |
| 业务 Cache.Drain(ctx) | 停止新调用，等待本 Cache 的已进入调用和在途 loader；不关闭共享后端 |
| HTTP NewWithStore | 借用 Store；Stager.Close 不关闭注入引擎 |
| HTTP 原 New 的 memory 路径 | 自己创建私有 Ristretto Owner，Stager 最终释放该 Owner |

Owner.Close(ctx) 采用 active → closing → closed：进入 closing 后不接收新操作；ctx 只约束尚未取得操作权的等待，超时保留 closing，后续可再次等待；原生释放最多一次。已经进入的原生 Close 不能被 context 强制打断。关闭后的读写删除返回 ErrClosed；统计只返回关闭标志和保存的计数，不访问已释放对象。

正常关停：停止 HTTP/业务/worker 入口 → 等待 HTTP 与业务调用 → Drain 所有业务 Cache → 关闭包装与视图 → 关闭拥有的 Owner → 原创建者关闭借用裸引擎。Drain 超时不等于使用者已退出，不能据此释放仍在使用的共享对象。启动失败只回收本次成功创建且拥有的资源；不关闭借用对象。

一致性范围：同一 Store 已完成操作遵守上述顺序；原生淘汰允许 miss。数据库与业务缓存、业务缓存与 HTTP 缓存仍需分别失效，跨进程也无新增广播。未成功失效的旧值可持续至原逻辑 TTL；没有业务最大 TTL 输入就不填写虚假的全局最大不一致秒数。

## 9. 版本、文件和迁移范围

| 依赖 | 拟处理 |
|---|---|
| github.com/allegro/bigcache/v3 v3.2.0 | 只加入 gin_api；候选源码已核对，Go 1.22 基线，不代表编译/安全检查通过 |
| github.com/coocood/freecache v1.2.7 | 只加入 gin_api；候选源码已核对，Go 1.21 基线，不代表已安装 |
| github.com/dgraph-io/ristretto/v2 v2.4.2 | gin_api 沿用现有锁定版本；本次再次读取本机该版本源码 |
| github.com/gtkit/gincache v1.3.1 | gin_api 继续用现有公开接口；实施前核对实际发布包内容，不仅看工作区 |
| 旧 ristrettoadapter v1.1.0 | gin_api 全部引用迁移后移除直接依赖；gincache 仓库旧模块保留 |
| gtkit/json/v2 v2.1.1 | gin_api 适配/codec 沿用；不引入 encoding/json |

未来 gincache 运行代码、依赖、子模块、Makefile 无需变更。可在实现完成时更新根 README 的调用方适配示例及旧适配模块定位；不混入其它 README 修复、发布或版本迁移。

未来 gin_api 新增 internal/pkg/cacheadapter 内的 owner、store、record、三种引擎文件及对应测试；业务桥接、HTTP 注入和装配按配套文档文件清单。没有新适配模块、独立 go.mod、发布版本或永久本地 replace。

默认业务 New 继续 LRU；默认 HTTP New 继续 Ristretto/Redis 选择及已有配置。memory 路径改用本地 Ristretto 包装，记录、命名空间和按字节计费更明确，有效条目数量可能变化，不承诺命中率完全相同。所有本地数据可冷启动，不搬运旧编码。

## 10. 验收目标（全部待执行）

| ID | 可复核结果 |
|---|---|
| A01 | 三种真实引擎各自实现现有 CacheStore、LocalStore，单级 HTTP 与 TwoLevelStore L1 均能往返 |
| A02 | 同 Owner、同 logicalKey、不同 namespace 不串读/覆盖/互删；关闭或重置统计不影响另一视图 |
| A03 | namespace 边界、Unicode、冒号、空/负/亚秒 TTL、到期等号和默认回落符合契约 |
| A04 | 短 L1 TTL 不被放大；逻辑过期不依赖原生清扫；业务抖动只计算一次 |
| A05 | slice 复制、记录损坏、完整键校验、确定性原生哈希冲突不泄漏其它键的值 |
| A06 | FreeCache 单条限制、BigCache 原生写错、Ristretto 缓冲/策略拒绝分别按约定返回 |
| A07 | Ristretto Wait 后核对的是本次序号；Set/Delete 顺序、连续覆盖、关闭不产生旧值复活 |
| A08 | nil/typed-nil、重复 namespace、重复/借用关闭、启动失败和关闭交错不 panic、不越权释放 |
| A09 | 共享只共用总预算；独立实例没有跨实例淘汰；统计口径与 O(N) 成本如实标注 |
| B01 | 原业务 API、默认 LRU、取消、singleflight、超时、在途限制、失效屏障和 nil T 行为保留 |
| B02 | 字节 codec、损坏记录、正常拒绝、错误 bypass、Drain 成功/超时/再次等待有覆盖 |
| B03 | HTTP 默认关闭、身份绕过、200/Vary/信封/body 上限以及 Redis 路径不回归 |
| B04 | 注入 Store 不被 Stager 关闭；默认私有 Ristretto 由原 New 释放；公开 Factory 接线不扩大资源注册接口 |
| X01 | 每种引擎各跑业务+HTTP 的共享/独立两种接法，覆盖失效和关停 |
| X02 | gincache 未新增三种原生依赖；gin_api 不依赖旧适配 module；发布态 gincache 接口可编译 |

测试拟使用每实例 16 MiB、小于 4 KiB 的正常 payload，另建超限用例；这些是开发输入，不是生产容量建议。真实性能、RSS、吞吐与关闭预算须另行测量。

## 11. 分阶段实施计划

保留 S0–S8 总体顺序：S2/S3 的交付位置从 gincache 改到 gin_api，S3 增加 Ristretto 子步骤；与旧基础计划 P1–P6 分开，不重排或自动恢复旧授权。

| 步骤 | 前置条件 | 具体改动与交付物 | 检查方法与验收证据 | 停止条件 | 状态 |
|---|---|---|---|---|---|
| S0.2 | 本次文档授权 | 更新两份原方案，替换适配归属与依赖决定 | 全文回读、链接及跨文档契约核对 | 触及非目标文件 | 已完成文档更新与回读 |
| S1.1 | 后续明确实施授权 | 重读本方案、配套方案、旧计划、错误记录与最新源码，确认并发工作及旧 P1–P4 实际状态 | 实际文件/符号/状态记录 | 状态冲突、旧计划不完整或需扩大授权 | 已完成前置核对 |
| S1.2 | S1.1 完成 | 核对三种版本/包名、发布态接口；固定私有适配契约和所需依赖 | 官方/本机源码依据、确切版本与文件清单 | 版本不兼容、需升级 gincache 或需新生产权限 | 已完成前置核对 |
| S2.1 | S1 完成 | gin_api 新增公共 Owner/Store/记录及 FreeCache 适配，借用与自建均支持 | A01–A06/A08/A09 中相关普通用例 | 串键、过期命中、超限假成功、影响其它视图 | 已完成普通功能验收 |
| S3.1 | S2 契约固定 | 同包新增 BigCache 适配、哈希冲突删除保护 | 真实 BigCache 的 A01–A06/A08/A09 | 只用全局 TTL 或删除其它完整键 | 已完成普通功能验收 |
| S3.2 | S3.1 完成 | 同包新增 Ristretto 适配、序号确认、Set/Delete 屏障 | 真实 Ristretto 的 A01–A09 | 以 Set=true/Wait 冒充准入、死锁或异步复活 | 已完成普通功能验收 |
| S4.1 | 旧 P1–P4 已完成核对；缺失部分另获准按旧步骤补齐 | 新增 ByteStore/Codec/ByteBackend 和 Drain | B01/B02，三种引擎对照默认 LRU | 默认 T 语义改变、失效屏障或退出计数缺失 | 已完成普通功能验收 |
| S5.1 | S2–S4 完成 | HTTP NewWithStore；默认 Ristretto 切本地适配；bootstrap 窄 Factory 接线 | B03/B04，默认与注入、启动失败、关闭证据 | 准入变更、默认开启或借用对象被关闭 | 已完成普通功能验收 |
| S6.1 | S2–S5 完成 | 三种引擎共享与独立的跨消费集成 | X01 和各引擎原生路径日志 | 把总预算当配额、使用者未退出就释放 | 已完成普通功能验收 |
| S6.2 | S6.1 完成 | 确认旧 module 已无引用，再精确整理 gin_api 依赖 | X02，版本差异与普通 build 证据 | 引发无关升级、修改 gincache 依赖或遗留 replace | 已完成普通功能验收 |
| S7.1 | 全部功能完成 | 同步两项目相关 README、gin_api 配置说明/模板和既有发布记录 | 逐项对照签名、默认值、容量和关闭语义 | 尚未验证却写成完成，或扩大文件范围 | 已完成普通功能验收 |
| S8.1 | S7 完成 | 单独询问受控检查范围 | 用户明确授权记录 | 无许可不得执行聚合入口 | 已获授权 |
| S8.2 | 对应检查已获准 | 仅运行获准 vet/lint/modern/race/覆盖率与依赖安全检查；性能另列 | 本轮命令、环境、退出码和日志 | 用户叫停、工具不符或范围扩大 | 已完成检查及归档 |

普通功能阶段候选为 gin_api 相关包的 go test -vet=off 与 go build；实际执行前核对测试依赖，禁止隐式容器拉取。两级集成可复用已有内存 Redis fixture，但不把它当真实 Redis 服务证明。本轮不运行以上命令或 OpenSpec；未来实施才在当时授权内处理工作流。

## 12. 回滚与推广

开发回滚只撤本任务 patch，保护其它改动，不 reset --hard。启用按测试环境独立 → 测试环境共享 → 经调用方批准的少量实例 → 扩大；观察 miss/拒绝/存储错误、业务与 HTTP 命中、回源、P99、RSS 和未结束 loader。

任一串键、超期返回、失效后复活、关闭仍被使用的对象、HTTP 准入回归，立即停止推广。P99/RSS/回源阈值目前没有真实基线，生产推广未就绪，不能自行填写 SLA。

回滚先停入口、等待使用者和 Drain，再释放 Owner；恢复默认 LRU 与默认 HTTP 构造路径，使用空缓存。若回滚的是整个适配 patch，恢复对应原锁定依赖及旧默认适配；不把新旧依赖随意混配。无持久缓存格式迁移；冷启动的回源增加需观察。发布、部署和生产配置操作另行授权。

## 13. 文档交付与历史记录

- 本次写入范围仅本文件和 gin_api 配套方案。原基础计划保留；代码、依赖、配置和 OpenSpec 未修改。
- 旧版曾两次猜错响应缓存路径，后按清单核实为 internal/platform/responsecache/owner.go；该历史记录保留，不作为当前实现缺陷或新的测试结果。
- 修订前已存在 gincache 依赖文件与 gin_api 基础计划的工作区修改；不归因于本轮，也不覆盖。
- 两份修订文档已分段全文回读，相互文档链接均指向实际文件；相关读取命令退出码为 0。旧基础计划的 SHA-256 与修订前一致。所有运行验收保持“未执行”。

## 14. 官方依据

固定版本源码用于 API/语义依据，不等同于安装、编译、性能或安全验证：

- [BigCache v3.2.0](https://github.com/allegro/bigcache/releases/tag/v3.2.0)、[配置](https://github.com/allegro/bigcache/blob/v3.2.0/config.go)、[原生操作](https://github.com/allegro/bigcache/blob/v3.2.0/bigcache.go)、[分片删除](https://github.com/allegro/bigcache/blob/v3.2.0/shard.go)
- [FreeCache v1.2.7](https://github.com/coocood/freecache/releases/tag/v1.2.7)、[原生操作](https://github.com/coocood/freecache/blob/v1.2.7/cache.go)、[条目与期限限制](https://github.com/coocood/freecache/blob/v1.2.7/segment.go)
- [Ristretto v2.4.2 Cache、Wait、IterValues](https://github.com/dgraph-io/ristretto/blob/v2.4.2/cache.go)、[原生存储](https://github.com/dgraph-io/ristretto/blob/v2.4.2/store.go)

## 本轮实施授权与 S1 记录（2026-10-23）

- 用户最新授权：按两份落盘方案开始分阶段实施，保留现有功能。S2–S7 按序执行，受控检查与发布不在本轮默认授权中。
- 两份方案需要的业务基础接口尚未实现；在本轮变更内按旧 P1–P4 顺序补齐必要前置，不修改旧计划或旧 OpenSpec change，也不把其状态写成已完成。
- 独立 OpenSpec change：add-caller-owned-local-cache-adapters；通过项目已有 `.openspec-auto/tools/run_openspec.py` 执行。直接 openspec 不在 PATH 的失败记入 ERR-20261023-141，未安装工具。
- 本机 `/usr/local/bin/go version`：go1.27.3 darwin/amd64。固定版本 BigCache v3.2.0、FreeCache v1.2.7 已经 go mod download 取得并读取实际源码；Ristretto 沿用 v2.4.2。尚未改动项目依赖声明。
- 原 cache 基线：`GOTOOLCHAIN=local /usr/local/bin/go test -vet=off -count=1 -timeout=120s ./internal/pkg/cache`，退出 0，输出 `ok github.com/gtkit/gin-api/internal/pkg/cache 0.842s`。没有执行 vet、lint、modern、race 或覆盖率。
- 未提交的文档及其它任务改动保留；不创建新 worktree、不调整现有目录，不接手无关 change。

### S2 公共层与 FreeCache

- 已实现 Owner/Store/GAC2/FreeCache；JSON 编码时间计入逻辑 TTL。
- 普通测试：`go test -vet=off -count=1 -timeout=120s ./internal/pkg/cacheadapter` 通过；本机 Go 1.27.3，exit 0。
- `go build ./internal/pkg/cacheadapter` exit 0。已覆盖隔离、精确期限、复制、损坏、构造边界、取消、借用和重复关闭。性能、race 和覆盖率未执行。

### S3 三种原生适配完成

- BigCache、Ristretto 已接入本地适配包。BigCache 增加真实哈希冲突和超大条目错误用例，Ristretto 增加准入拒绝、连续写删与借用关闭用例。
- `go test -vet=off -count=1 -timeout=120s ./internal/pkg/cacheadapter`：Go 1.27.3，exit 0（1.720s）；`go build ./internal/pkg/cacheadapter` exit 0。未运行受控检查。
- S4/P1：原代码上两个确定性反例按预期失败（exit 1），用于后续验证失效屏障与合法 nil 修复；原有基线已在 S1 通过。

### S4–S7 实施及普通功能验收（2026-10-23）

- S4：补齐 Record/Backend、默认 LRU、NewWithBackend、InvalidateErr/bypass、类型化 singleflight 结果，并加入 ByteBackend/JSONCodec 与 Drain。原两个反例在修复后通过，原 cache 普通测试继续通过；默认引用语义与可取消等待保留。
- S5：NewWithStore 借用构造、默认 Ristretto 本地适配、bootstrap.Options.ResponseCacheFactory 已接通；默认/注入/冲突/失败清理和原 HTTP 准入测试通过。
- S6：三种真实引擎各运行共享/独立两组业务+HTTP 用例，以捕获的真实 HTTP 缓存键作为业务逻辑键验证隔离；L1 使用 miniredis fixture 验证短 TTL 与共享 Owner 关闭边界。fixture 不作为真实 Redis 服务或生产证明。
- 依赖仅在 gin_api 加入 BigCache v3.2.0、FreeCache v1.2.7，保留 Ristretto v2.4.2，移除旧 ristrettoadapter v1.1.0。`go mod tidy -diff` 的 exit 1 表示存在预期依赖差异；阅读差异后执行 `go mod tidy`，exit 0，无其它版本升级。gincache 的已有依赖修改不属于本轮。
- S7：同步 adapter README、两项目 README、docs/cache.md、docs/internal-pkg.md、配置注释与 config/env.yml.example。依照已读取的能力清单约定，只在现有 SCAFFOLD.md 登记新 cacheadapter，不调整架构或启用配置。

最终普通功能命令（本机 Go 1.27.3 darwin/amd64）：

```text
GOTOOLCHAIN=local /usr/local/bin/go test -vet=off -count=1 -timeout=120s ./internal/pkg/cacheadapter ./internal/pkg/cache ./internal/pkg/httpcache ./internal/platform/responsecache ./internal/bootstrap
ok github.com/gtkit/gin-api/internal/pkg/cacheadapter 1.420s
ok github.com/gtkit/gin-api/internal/pkg/cache 2.535s
ok github.com/gtkit/gin-api/internal/pkg/httpcache 0.909s
ok github.com/gtkit/gin-api/internal/platform/responsecache 0.781s
ok github.com/gtkit/gin-api/internal/bootstrap 5.436s
exit 0

GOTOOLCHAIN=local /usr/local/bin/go build ./internal/pkg/cacheadapter ./internal/pkg/cache ./internal/pkg/httpcache ./internal/platform/responsecache ./internal/bootstrap
exit 0

python3 .openspec-auto/tools/run_openspec.py validate add-caller-owned-local-cache-adapters --type change --strict --json --no-interactive
valid=true, passed=1, failed=0, exit 0
```

回审范围：新适配器全部方法、业务 GetOrLoad/失效/Drain、HTTP 构造及 bootstrap factory 的直接调用链；使用文本引用检索与源码，没有把它当作完整语义调用图。未验证其它构建标签、平台、仓库外调用者或生产流量。容量/RSS/P99 未测，不作生产准入结论。

S8 的 vet、lint、modern、race、覆盖率及依赖安全检查尚未执行，需功能完成后单独授权。当前 change 保留待检查状态，未归档、提交、推送或发布；旧基础计划/change 未写入。

- S8.1：已通过当前会话询问是否运行受控检查；等待用户选择。S8.2 尚未执行。gin_api 的 CHANGELOG.md 明确仅记录已发布版本，本轮未发布，故未改写该文件。

### S8 授权与执行边界（2026-10-23）

- 用户已明确回复“进入 S8，运行上述检查”，授权 vet、lint、modern、race、覆盖率及依赖安全检查。
- 范围固定为 gin_api 的 cacheadapter、cache、httpcache、platform/responsecache、bootstrap 五个受影响包；gincache 只有本轮文档变更，不额外运行其仓库门禁。
- 本机 Go 1.27.3、golangci-lint 2.15.0 与项目声明一致；govulncheck 使用 go.mod 锁定工具，经 `go tool govulncheck` 调用。
- 拟执行：`go vet -tags=scaffold <五包>`、项目配置下的 `golangci-lint run <五包>`、`go fix -diff -tags=scaffold <五包>`、`go test -vet=off -tags=scaffold -race -count=1 -timeout=5m -covermode=atomic -coverprofile=<本轮日志目录>/coverage.out <五包>`，以及 `go tool govulncheck -tags=scaffold <五包>`。
- coverage 同时记录逐包与聚合值；项目 Makefile 的 72.5% 为全仓基线，本轮五包结果只代表此次范围，不宣称全仓覆盖率。modern 须退出 0 且无差异输出。
- 若发现本次代码缺陷，先记录事实并在原范围内修复、复测；存量或范围外问题如实列出，不擅自升级依赖或扩大改动。记录结果后再验证、归档本次 OpenSpec change；不提交、推送、发布，不执行 Docker 或未获准的聚合门禁。

S8 首轮结果与修复步骤：vet、modern、五包 race/coverage 均退出 0；lint 退出 1，报告 13 项 errcheck 与 9 项 G115，已记 ERR-20261023-145。govulncheck 退出 0，无可达符号/导入包漏洞，仍需展开一个未调用的模块级通告。

- 在既有 backend_lru.go 中显式校验链表元素类型，异常返回错误供已有 bypass 处理；Invalidate 保留 void 签名并显式处理已有错误；Example 显式处理打印错误。
- 保持 GAC2/业务记录格式：Unix 秒按原 64 位有符号位模式存储，纳秒来自 time.Time 的有界返回值。仅对这两种有依据的转换作逐行说明；键长度改为先绑定、校验再转换，消除可避免的告警。
- 在已有两个测试文件补充负 Unix 秒、纳秒边界、精确到期的记录往返验证；不修改无关包和规则配置。完成后复跑五包 lint、vet、modern、race/coverage，并展开同范围漏洞详情。

### S8 最终检查证据（2026-10-23）

- 本机 Go 1.27.3 darwin/amd64、golangci-lint 2.15.0、go.mod 锁定的 govulncheck v1.9.0；检查范围为 gin_api 五个受影响包。
- 最终 build、vet、lint、modern、race/coverage 均 exit 0；lint 0 issues，modern 无差异，race 无报告。首轮 lint 22 项已修正，六处有符号时间/有界纳秒的局部 G115 说明保留兼容格式，并有时间边界往返用例。
- 五包聚合 statement coverage 为 85.6%，对照 72.5% 的检查命令 exit 0。逐包：cacheadapter 85.1%、cache 90.0%、httpcache 94.9%、platform/responsecache 98.4%、bootstrap 82.9%。这不是全仓覆盖率。
- 含测试的 govulncheck 扫描 121 个模块和 Go 标准库，exit 0，无可达符号或导入包漏洞；既有 x/crypto v0.58.0 的 openpgp 模块级通告 GO-2026-5932 不在该导入/调用路径，修复版本 N/A。三种引擎本轮无通告，未为范围外依赖做升级。
- 完整命令、标签差异、日志链接和首次失败记录见 [gin_api 方案 S8 证据](../../Project/gin_api/docs/cache-sharing-adapters-plan-2026-10-23.md)。本仓未改引擎依赖或运行代码；未运行全仓 make check、真实 Redis/生产容量/RSS/P99、其它平台、容器或发布操作。
- 最终 OpenSpec 严格校验 exit 0（1 passed / 0 failed），归档 exit 0；已确认活动目录移除、归档目录 `2026-10-23-add-caller-owned-local-cache-adapters` 存在，7 条 requirement 合入 gin_api spec，tasks 无未完成项。旧基础 change 未改写；两份方案已同步最终状态。
- 已补齐合并后 spec 的 Purpose，单独对 `caller-owned-local-cache` 执行严格 spec 校验，exit 0（1 passed / 0 failed），无遗留占位文字。
