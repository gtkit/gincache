# 可选缓存适配模块与 gin_api 撤回计划

日期：2026-10-23。状态：M1–M5 已完成；受影响范围校验通过，两个本轮 OpenSpec change 已实际归档。主库两个既有测试失败另列，不冒充全量通过。

## 授权与目标

用户要求先删除上一轮在 gin_api 添加的代码，再在 gincache 做最小依赖；随后明确选择“撤销上轮整套改造，恢复原有功能”。本计划取代 `CACHE_ADAPTERS_PLAN_2026-10-23.md` 的实施方向，旧文档和历史证据保留。当前许可覆盖下述 M1–M4；M5 的受控检查须功能完成后另行询问，不沿用上轮 S8 授权。

开发恢复阶段边界（已结束）：用户要求“所有功能完成前，不要进行任何校验，一次性完成所有开发再停下问我是否需要校验”，随后要求继续完成两仓全部开发。该阶段未运行测试、构建、vet、lint、modern、race、覆盖率、安全扫描、OpenSpec 校验或归档；开发完成后已重新询问并获得 M5 授权。下文 M1–M4 的执行结果属于历史证据，最新检查结果见 M5。

目标：gin_api 恢复原来的 LRU、HTTP Ristretto/Redis 及构造入口；gincache 主模块不引入任何原生缓存引擎；BigCache、FreeCache、Ristretto 分别由独立可选 Go module 提供，调用者只引入所选引擎。共享实例、命名空间、TTL 和资源所有权仍由适配层明确处理。

非目标：现在重新给 gin_api 接线、全局插件注册、配置枚举、动态加载、性能优化、发布/推送/打 tag、升级第三方引擎、改写旧 OpenSpec 历史或其他工作区改动。

## 已核对事实与边界

- gincache 主模块为 `github.com/gtkit/gincache`，Go 1.27，JSON 已依赖 `github.com/gtkit/json v1.0.0`。现有 CacheStore、LocalStore 无需新增方法。
- 已有 `persist/ristrettoadapter` 独立模块及 `persist/ristrettoadapter/v1.1.0` 本地 tag；旧 New、Store、Option、WithLocalStore 和行为必须保留。其已有 go.mod/go.sum、Makefile、version.go 等工作区修改不是本轮创建，禁止整体覆盖。
- gin_api 这轮可撤回内容已逐个对照 Git diff：13 个已有文件与 23 个新增文件。其他 harness 文档、旧 cache-adapter-design-plan、配置真实值和其他模块不属于撤回范围。
- 旧 cache.go 中合法 nil 和失效/提交竞态的修正属于被撤回的整套改造；按用户选择恢复旧实现，不声称这些修正仍保留。
- 本地 OpenSpec CLI 已安装，gincache 没有 run_openspec.py；复用已核实的 gin_api wrapper 的绝对路径，在目标仓库 cwd 执行，禁止安装或修改工具。
- 尚无生产 QPS、RSS、P99 与真实业务类型数据，不据此承诺容量收益或生产准入。

## 依赖与文件设计

1. 主模块新增 `persist/internal/localadapter/{owner,store,record}.go`：复用上一轮无引擎公共实现，JSON 改用本模块已有 gtkit/json。内部 Engine 仅在该 internal 范围导出所需方法，主模块不导入任何引擎。共享测试辅助只被测试引用。
2. 新增 `persist/bigcacheadapter` 与 `persist/freecacheadapter` 两个 Go module：分别只依赖 BigCache v3.2.0 或 FreeCache v1.2.7，以及本仓主模块。包括适配源码、GoDoc、契约测试、Example、README、CHANGELOG、version.go、go.mod/go.sum。
3. 既有 `persist/ristrettoadapter` 增加独立 owner 实现文件与测试，沿用 Ristretto v2.4.2；保留旧 adapter.go 与已有测试的内容。新增 API 不替换旧 New。
4. 三个模块提供 `NewOwner` / `WrapOwner`、Owner、NamespaceStore、StoreOption、WithStoreExpiration、WithClock 和必要错误标识。Owner.NewStore 返回实现现有 CacheStore/LocalStore 的视图；旧 Ristretto Option 与新 StoreOption 分离，避免同名签名破坏。
5. 主模块 README/CHANGELOG/Makefile 仅更新可选模块说明与显式子模块列表；不改变默认主模块依赖。新增内部公共实现需主模块下次 MINOR 1.4.0；Ristretto 新 API 对应下次 MINOR 1.2.0；新模块起始 0.1.0，仅更新版本元数据，不创建 tag。
6. 子模块依赖新主模块 v1.4.0。它尚未发布，开发验证使用临时 go.work 或 -modfile 指向本地模块，不在正式 go.mod 中留下 replace、不修改用户已有 go.work。提供不自动生效的 go.work.example 供本地复现，例子中的版本限定 replace 仅用于未发布源码开发。正式分发须先发布主模块再发布适配模块，本轮不执行发布，不能声称可从代理安装未发布版本。

## 兼容、TTL、容量和失败处理

保留上一轮 GAC2 29 字节头、完整物理键、写入序号、namespace 长度前缀和按绝对时间到期的语义。0 TTL 使用视图默认值，负值拒绝，命中不续期；原生早淘汰只产生 miss。正常准入拒绝不等于错误，Ristretto 在屏障后核对本次记录，BigCache 删除前核对完整键，FreeCache 保留原生单条限制。共享总容量没有 namespace 硬配额。

NewOwner 拥有资源；WrapOwner 借用；视图 Close 不关闭 Owner；所有消费者退出后由创建方 Close Owner。原生同步调用无法被 context 强制中断。单个原生对象只包装一个 Owner，不旁路修改。旧 Ristretto New 路径保持原有语义，新的 GAC2 视图不与旧 Store 共用同一 namespace/keyspace。

## 分阶段步骤

| 阶段 | 前置条件 | 具体步骤与交付物 | 验收证据 | 停止条件 | 状态 |
|---|---|---|---|---|---|
| M1 撤回 | 用户已确认全部撤回；读过 diff | 将本轮改动与 Git HEAD 的已核实前态备份到临时目录；恢复 13 个已核实文件，删除 23 个本轮新增文件；保留历史计划并标为被取代 | 逐文件回读/哈希、原业务/HTTP/bootstrap 普通测试与 build；无本地 cacheadapter、新 API、BigCache/FreeCache 引用 | 发现非本轮修改或备份不完整时暂停该文件 | 已完成 |
| M2 公共内核 | M1 完成，OpenSpec apply-ready | 无引擎 internal 内核、可复用的测试契约和错误路径测试；保留现有主模块 API | 普通测试覆盖隔离/复制/TTL/取消/关闭/损坏/序号；主模块依赖清单无三种引擎 | 引入引擎或改变已有主 API | 已完成普通验证 |
| M3 三个模块 | M2 完成 | 两个新独立模块，旧 Ristretto 增量 owner API；迁移原生边界用例，增加实际 gincache 中间件与 L1 适配验证 | 各模块普通测试/build；旧 Ristretto 测试继续通过；每个隔离 workspace 的依赖图只含对应引擎 | 旧 API 改签名、串键、超期命中、重复关闭或跨引擎依赖 | 已完成普通验证 |
| M4 文档交付 | M1–M3 完成 | 文档、版本、模块列表一致；两个仓库 OpenSpec 严格校验；记录真实命令和未验证项 | README 接线、模块依赖、版本和实际源码一致；工作区无永久 replace | 未发布写成已发布、未执行写成通过 | 已完成 |
| M5 统一校验 | 两仓开发全部完成后重新询问并获授权 | 按新授权运行测试/build/vet/lint/modern/race/coverage/security/OpenSpec 校验；满足条件后归档本轮 change | 新一轮日志和真实退出码，历史失败另列 | 用户尚未重新授权时不启动任何校验 | 已完成；旧失败单列 |

普通验证只用本机 Go，`go test -vet=off -count=1 -timeout=120s` 与 `go build`；不调用 Makefile 聚合入口，不拉镜像、不创建容器。相应测试不得访问生产配置和服务。以上是历史执行方式及后续候选命令，本次恢复开发不执行，须在开发全部完成后重新获准。

## 撤回清单与回滚

gin_api 恢复：README.md、SCAFFOLD.md、config/README.md、config/env.yml.example、docs/cache.md、docs/internal-pkg.md、go.mod、go.sum、internal/bootstrap/app.go、internal/bootstrap/runtime.go、internal/pkg/cache/cache.go、internal/pkg/httpcache/cache.go、internal/platform/config/cache.go。每个文件先以当前内容备份，再仅撤销已核实的本轮 diff；如存在并行修改不整体覆盖。docs/cache.md 已核实的响应准入事实说明保留，避免恢复过时描述。

gin_api 删除：internal/pkg/cacheadapter 的 12 个本轮文件；internal/pkg/cache 的 backend.go、backend_bytes.go、backend_bytes_test.go、backend_contract_test.go、backend_lru.go、backend_test.go、codec_json.go、drain_test.go；internal/bootstrap/cache_injection_test.go、internal/pkg/httpcache/injected_store_test.go、internal/platform/responsecache/shared_cache_test.go。

失败回滚：只使用本轮明确保存的文件和哈希恢复；不执行 reset/clean，不删除其他任务文件。新模块尚未对外发布，撤回新文件不会迁移生产缓存；旧模块源码和公开契约保留。OpenSpec 通过新 change 描述撤回，旧已归档 change 不改写。

## M1 实际记录

- 备份：`/var/folders/_w/xwv4k3t12vn9054592gsk5200000gn/T/gincache-adapter-migration-la1tbk7q`，含 current/baseline 和 SHA-256 manifest。13 个原文件恢复、23 个本轮文件删除已核对。
- gin_api 四包 `go test -vet=off -count=1 -timeout=120s`：exit 0；cache 1.049s、httpcache 0.763s、responsecache 1.492s、bootstrap 5.882s。相同四包 `go build` exit 0；本机 Go 1.27.3。
- internal 下已无 NewWithBackend/NewByteBackend/ResponseCacheFactory/cacheadapter 引用；go.mod 无 BigCache/FreeCache，旧 ristrettoadapter v1.1.0 已恢复。其他未提交改动保留。

M2：公共内核普通测试 exit 0（0.899s）。首次 helper 缺导入/别名的编译失败记 ERR-20261023-096，已修正。新增 API 只在子模块暴露；主模块共享逻辑位于 internal。

### 当前验证边界

- 主库全量普通测试的两个旧删除场景失败；从 Git HEAD 导出改前源码、仅复用当前依赖，在独立临时目录运行同一目标也复现，退出 1（0.902s）；当前单独复现 exit 1（0.736s）。本轮未修改这些 persist 源码，停止重跑/修改该旧问题，记录 ERR-20261023-098。不可称主库全量测试通过。
- 未发布 v1.4.0 在初次 workspace 模式下仍触发版本解析；已改用临时 -modfile 的版本限定本地 replace 来生成子模块依赖与验证。所有正式 go.mod 均不含 replace；这些临时文件不提交。
- 迁移遗漏分别记录 ERR-20261023-096/100/101：测试 helper 导入/别名、Ristretto 内部 RecordKey 访问与 HTTP 策略显式注入，均保留原生产校验，不削弱旧接口。

## M3 实际记录

- 本机 Go 1.27.3，GOWORK=off，临时 -modfile 使用已核对的本地版本限定替换。三模块 `go test -vet=off -count=1 -timeout=120s ./...` 全部 exit 0：BigCache 1.894s、FreeCache 1.114s、Ristretto 2.681s（包含其旧 API 测试）。公共内核最终普通测试 exit 0（0.787s）。
- 所有模块 `go build ./...` exit 0。主模块以及每个隔离模块的 `go list -m all` 已逐项断言：主模块无三种引擎；BigCache 仅有 BigCache；FreeCache 仅有 FreeCache；Ristretto 仅有 Ristretto。
- 日志及结果：`/var/folders/_w/xwv4k3t12vn9054592gsk5200000gn/T/gincache-adapter-work-almpv02k`，模块依赖日志和 dependency-build-results.json 可复核。正式 go.mod 没有 replace，原生版本未升级。
- 子模块 go.sum 保留既有记录并补齐所需记录，Ristretto 的旧 adapter.go 和旧测试未改写。

## M4 交付记录

- 已同步主库和三个适配模块 README、CHANGELOG、版本元数据、Makefile 子模块清单与 go.work.example。实际模块 go.mod 均无 replace，主模块 go.mod/go.sum 未因本轮新增引擎而改写。
- 版本元数据为主库 1.4.0、BigCache/FreeCache 0.1.0、Ristretto 1.2.0，均未创建新 tag、未发布。
- 两仓本轮 OpenSpec strict validate 均 exit 0，各 1 passed / 0 failed。go.work.example 的 main module 清单已实际读取，正确包含主库与三个可选模块。
- gin_api 撤回后的 13 个目标文件（docs/cache.md 保留正确准入说明）与 23 个删除目标已再次核对。旧 Ristretto 源码/API和其他用户改动保留。
- 本轮尚未运行 vet/lint/modern/race/coverage/security；原 S8 结果不是本轮结果，不能复用为本轮通过证明。M5 待询问。

## 本次恢复开发交付状态

- 两仓范围内功能源码已齐备：gin_api 恢复原业务缓存与 HTTP 缓存装配；gincache 提供三个独立可选模块，保留旧 Ristretto API。没有待实现的功能步骤，不为了继续开发重复修改已完成代码。
- gin_api 的 `docs/cache-adapters-rollback-plan-2026-10-23.md` 在恢复时未找到，原因未确认；依据本文件的完整范围、已有 OpenSpec 和已保存的历史结果补齐该文档，不将补齐文本冒充原文恢复。记录为 ERR-20261023-102。
- 同步三个适配器说明，保留命名空间、TTL、共享容量、资源所有权和未发布版本说明。
- 本次仅进行了开发状态所需的文件读取和文档收尾，没有运行任何测试、构建或校验；未归档、提交、打 tag 或发布。M5 尚未执行，交付时重新询问。

## M5 本次统一校验（已完成）

用户在全部开发完成后明确授权：运行受影响范围的测试、构建、vet、lint、modern、race、覆盖率、依赖安全及 OpenSpec 检查；两个既有测试失败单独记录，不顺带修改。此授权更新前面的暂停状态。

范围为主库 `persist/internal/localadapter`、测试辅助 `persist/internal/adaptertest`、三个适配子模块全部包，以及 gin_api 的 `internal/pkg/cache`、`internal/pkg/httpcache`、`internal/platform/responsecache`、`internal/bootstrap`。不重复运行已确认失败的旧 TwoLevelStore 用例，不改其他工作区变化。

执行步骤：
1. 使用本机 Go 1.27.3，子模块以临时 modfile 解析未发布的本地主模块；记录命令、环境、退出码和日志。
2. 每个范围运行普通测试、build、vet、`go fix -diff`、lint，以及 race + atomic 覆盖率。测试辅助不作为生产覆盖率分母；公共内核与各适配模块单独报告，不将模拟 Redis 结果称为真实 Redis 验收。
3. 使用 gin_api 锁定的 govulncheck v1.9.0 扫描受影响导入图；gincache 无 lint 配置，显式采用 golangci-lint 2.15.0 默认规则；gin_api 沿用已有配置。
4. 若出现本次改动引入的问题，先记录真实错误、补齐修复步骤，再做范围内最小修正和受影响复验；既有或范围外问题只报告。
5. 两个本轮 OpenSpec change 严格校验，检查全部任务和结果后再按原计划处理归档；不得修改或归档其他 change。未通过的项目如实标记。

当前状态：已按授权完成检查，结果如下；无新增依赖或全局工具安装。

### M5 范围内修正 1（已完成并复验）

首轮 modern 对 `persist/internal/localadapter/store_test.go` 给出确定 diff：将 blockingEngine 字面量中嵌套的 `memoryEngine.values` 初始化改为 Go 1.27 支持的提升字段 `values` 初始化；不改变值、类型、方法或测试断言。依据为本机 `go fix -diff` 实际输出和已读取的现代 Go 1.27 说明。仅改这一行，普通测试/build/vet/lint/modern/race/coverage 复验受影响公共内核；三种适配器在修正后运行最终检查。任一语义变化或新增范围外问题停止该修正。原始失败日志保留，记录 ERR-20261023-104。

### M5 范围内修正 2（已完成并复验）

BigCache 首轮普通测试、race、vet、lint、modern 和安全扫描均通过，但语句覆盖率 76.3%，低于项目 80% 下限。仅在 `persist/bigcacheadapter/owner_test.go` 补充两类用例：公开 NewOwner/WrapOwner 对 nil、已取消上下文及非法有界配置返回可识别错误、拒绝包装不影响原实例；公开 WithClock/WithStoreExpiration 的零 TTL 默认值和精确到期行为。现有生产代码、原断言、依赖版本与门槛不变。复验 BigCache 的普通测试/build/vet/lint/modern/race/coverage；记录真实覆盖率及剩余分支，无法达到门槛或出现新语义缺口时如实报告。原始日志保留，记录 ERR-20261023-105。

### M5 实际检查结果

本次使用本机 Go 1.27.3、golangci-lint 2.15.0、gin_api 锁定的 govulncheck v1.9.0。全部命令、工作目录、非敏感环境设置、退出码、耗时和逐项日志位于 `/var/folders/_w/xwv4k3t12vn9054592gsk5200000gn/T/gincache-m5-20261023-uzng1rhv`；汇总为 `final-check-results.json`，引擎依赖核对为 `dependency-boundaries.json`。

| 范围 | 普通测试 / build / vet / lint / modern | race | 语句覆盖率 | 依赖安全 |
|---|---|---|---|---|
| 公共适配内核 | 全部 exit 0；modern 无 diff | exit 0 | 81.3% | exit 0，0 可达漏洞 |
| BigCache 独立模块 | 全部 exit 0；modern 无 diff | exit 0 | 92.1% | exit 0，未发现漏洞 |
| FreeCache 独立模块 | 全部 exit 0；modern 无 diff | exit 0 | 94.3% | exit 0，未发现漏洞 |
| Ristretto 独立模块（含旧 API 测试） | 全部 exit 0；modern 无 diff | exit 0 | 85.7% | exit 0，未发现漏洞 |
| gin_api 四包 | 全部 exit 0；modern 无 diff | exit 0 | 合计 85.3% | exit 0，0 可达漏洞 |

gin_api 四包覆盖率分别为 cache 94.2%、httpcache 95.2%、responsecache 98.4%、bootstrap 82.6%。公共内核覆盖率只计算生产内核，不把仅被测试引用的 adaptertest 算作生产代码；各子模块单独计量。

命令口径：普通测试 `go test -vet=off -count=1 -timeout=5m <范围>`，构建 `go build <范围>`，vet `go vet <范围>`，modern `go fix -diff <范围>`，race/coverage `go test -vet=off -race -count=1 -timeout=5m -covermode=atomic -coverprofile=<临时文件> <范围>`。主库和子模块 lint 显式 `--no-config`；gin_api lint 使用其 `.golangci.yml`。全部使用 `GOTOOLCHAIN=local`、只读模块模式；子模块通过临时 `-modfile` 解析未发布 v1.4.0，正式模块未增加 replace。

本次依赖图五项断言通过：主库无三种原生引擎；每个独立适配器仅含自身引擎；gin_api 只含原 Ristretto，没有 BigCache/FreeCache。主库与 gin_api 扫描另提示 `GO-2026-5932`（x/crypto/openpgp）：仅声明依赖层面，所扫代码未导入漏洞包、无可达调用，工具未提供修复版本；不据此升级依赖或声称所有依赖不存在公告。

两个本轮 OpenSpec change 的严格校验均 exit 0、各 1 passed / 0 failed。作用于本次文件的 diff 空白检查和 gofmt 检查无输出。没有重跑或修改 ERR-20261023-098 的旧 TwoLevelStore 两个失败；这些通过结论仅覆盖表中范围，不代表主库全量测试、真实 Redis 服务、生产容量或性能已经验收。

M5 仅修改了两处测试文件：公共内核的一个等价现代语法替换，及 BigCache 的构造/公开选项边界用例。未修改生产行为、旧 Ristretto 实现、gin_api 生产代码或正式依赖。归档只处理 `isolate-optional-cache-adapters` 与 `rollback-caller-cache-adapters`，最终结果见下文。

### M5 OpenSpec 归档修正（已完成并复验）

gincache change 已实际归档并合并 4 条规格。gin_api 首次 archive 虽 exit 0，但输出 `Spec must have at least one requirement`、`Aborted. No files were changed.`；未归档，不能记为完成。原 delta 删除全部 7 条新增要求，留下空规格，不满足工具约束。

仅在 gin_api 本轮 change 的 proposal/design/spec delta 中明确“删除旧注入要求并记录原装配恢复结果”，增加一条以用户回退要求、当前 cache.New[T] 和 HTTP Ristretto/Redis 源码为依据的恢复契约。不得恢复已撤回的新接口、添加新功能或改写旧归档。重新执行严格校验与归档，确认实际移动后同步旧 capability 的 Purpose，最终校验两个目标规格。若仍不能用真实实现表达有效规格，停止并报告，不跳过规格合并。记录 ERR-20261023-106。

最终归档结果：
- gincache：`openspec/changes/archive/2026-10-23-isolate-optional-cache-adapters` 已存在，原 active 路径不存在；`optional-local-adapters` 合并 4 条要求。
- gin_api：`openspec/changes/archive/2026-10-23-rollback-caller-cache-adapters` 已存在，原 active 路径不存在；移除 7 条旧要求、增加 1 条原装配恢复要求。首次空规格中止的日志保留，未跳过规格合并。
- 两个归档后的目标 spec 分别执行 `validate <name> --type spec --strict --json --no-interactive`，均 exit 0、1 passed / 0 failed；对应 Purpose 已与当前行为对齐。完整结果为 `final-openspec-state.json`。

合规记录：本次范围的 Go 1.27 modern、lint/vet、单元与 Example、race、覆盖率门槛、依赖隔离、旧 Ristretto API 及 OpenSpec 检查通过。共享 Owner 的测试覆盖取消、在途操作、关闭、TTL 与键隔离；结论限于实际执行的路径。Benchmark 已有代码，本次未运行性能测试，不承诺吞吐或 RSS；未连接真实生产服务，未提交、推送、打 tag 或发布。ERR-103/104/105/106 已处置；ERR-098 继续单列开放，不修改或重跑其两个旧失败。
