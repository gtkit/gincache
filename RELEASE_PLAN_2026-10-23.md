# 可选缓存适配模块发布计划

日期：2026-10-23。状态：主模块与三个子模块已通过附注标签发布并推送到 GitHub，发布版本的按需安装及独立/共享示例均验证通过。仅豁免两个已记录的旧失败。

## 来源、范围和发布准备前事实

用户最新指令为“提交，发版并 push 到 GitHub”。本计划在已完成的
`CACHE_ADAPTERS_MINIMAL_PLAN_2026-10-23.md` 基础上追加发布步骤，不改变原有功能、接口、目录或依赖方向。

- 本次只操作 gincache 仓库，分支 `main`，远端 `gtkit` 为 `git@github.com:gtkit/gincache.git`；不提交或推送 gin_api。
- 准备前本地 HEAD 与远端 main 均为 `991ef1a61a49c1ce520c732ee1427d0a5354d6ec`；最新主模块标签为 `v1.3.1`，已有 Ristretto 标签为 `persist/ristrettoadapter/v1.1.0`。
- 提交当前缓存改造、四份 README、版本元数据、两份既有设计/实施文档、go.work.example、Makefile 子模块列表，并保留和提交当前工作区已有的依赖/锁文件更新及 ignore 文件；不额外升级依赖，不提交临时日志、工作区文件、凭据或忽略目录中的内容。
- 已有工作区原生引擎版本为 BigCache v3.2.0、FreeCache v1.2.7、Ristretto v2.4.2；主模块 Go 1.27、JSON v1.0.0、Redis v9.23.0，保持现有取值。各正式 go.mod 不添加 replace。
- M5 已完成受影响范围的测试、构建、vet、lint、modern、race、覆盖率、依赖安全和 OpenSpec 校验；命令、退出码与范围见原实施计划。上述结果是历史证据，不等于本次重新执行或全仓门禁通过。
- `ERR-20261023-098` 的两个旧用例仍失败：`TestTwoLevelStoreBackfillDoesNotResurrectMutatedKey` 下的 Delete、DeletePattern 场景；改前基线已复现。按用户指令不顺带修改或重跑这两个用例。
- 最新补充的 README 完整示例尚未编译运行。尚无全仓发布门禁、API 差异工具检查或 benchmark 的本轮通过证据。
- GitHub API 查询旧 HEAD 的 check-runs 返回 0，发布列表为空；这只说明没有查到记录，不等于 CI 通过。Git SSH 远端读取成功。未安装可调用的 gh，尚无创建 GitHub Release 页面的已验证路径。

非目标：修复旧测试、修改运行行为或原有接口、更新原生引擎版本、重做 OpenSpec、添加 CI、调整发布脚本、操作 gin_api、创建 v2 版本线。

## 目标版本与兼容约束

| 模块 | 版本常量 | Git 标签 |
|---|---|---|
| 主模块 | v1.4.0 | v1.4.0 |
| BigCache | v0.1.0 | persist/bigcacheadapter/v0.1.0 |
| FreeCache | v0.1.0 | persist/freecacheadapter/v0.1.0 |
| Ristretto | v1.2.0 | persist/ristrettoadapter/v1.2.0 |

旧 Ristretto New/Store/Option 及主模块既有接口保留。新 Owner/namespace API 为可选增量。
主模块无三种原生引擎依赖。三个子模块依赖主模块 v1.4.0，因此先发布主模块，待其可解析后再发布子模块。
子模块标签必须带目录前缀；不能直接运行会自增现有版本或生成无前缀标签的通用模板。

## 阶段与验收

| 阶段 | 前置条件与步骤 | 可复核证据 | 停止条件 | 当前状态 |
|---|---|---|---|---|
| R1 提交与推送 | 读取规则、原计划、旧失败记录、当前 diff 和远端引用；回读本计划；按明确文件清单提交；正常推送 main | 暂存文件清单、commit SHA、push 返回及远端 main SHA | 非预期文件、并行改动、远端前移、认证或 hook 拒绝 | 已完成，见下方实际记录 |
| R2 发布门禁决定 | 功能已完成；受控检查须用户单独授权。先说明旧失败与尚缺证据；由用户决定补跑发布门禁及旧失败例外，或明确采用现有范围检查作为发布依据 | 用户明确指令；获准后记录真实命令、退出码和被接受的例外 | 未获得对应检查授权或发布例外；新失败未经处理 | 已完成，仅两个旧失败获准豁免 |
| R3 主模块发布 | R2 条件满足；将 CHANGELOG 的 Unreleased 归入 1.4.0 和实际发布日期，同步 README 发布状态，提交并推送；确认工作区干净及远端标签不存在后创建附注标签并正常推送 | 版本常量、CHANGELOG、标签类型及解引用 SHA、远端引用 | 门禁未满足、版本或文档不一致、已有同名远端标签 | 已完成，含真实下载和校验 |
| R4 子模块发布 | 主模块 v1.4.0 已可解析；按实际下载结果补齐子模块 go.sum，不升级依赖；将三份 CHANGELOG 归入对应版本并同步 README，提交并推送；创建三个带路径前缀的附注标签 | 每个模块的真实解析结果、锁文件 diff、版本与标签对应关系、远端引用 | 主模块不可解析、依赖意外变化、标签冲突 | 已完成，三个附注标签已推送 |
| R5 交付核对 | 所有拟发布引用已推送；核对远端指向和安装解析结果，报告已完成与未完成事项；GitHub Release 页面与 Go 模块标签发布分开记录 | GitHub commit/tag 链接、模块解析证据；页面未创建时明确说明 | 无法核实的结果不得报告成功 | 已完成实际安装和运行验证 |

发布检查按实际脚本及项目规则拆开执行，不通过聚合入口间接运行尚未授权的 vet、lint、modern、race 或覆盖率检查。
README 和 CHANGELOG 已随发布阶段更新；是否发布以实际远端标签与下方执行记录为准。

## 风险、失败处理与回滚

- 两个旧失败与完整门禁缺口阻止按默认规则直接打标签；不能将局部测试通过改写成全仓通过。
- 主模块先发布、子模块后补齐实际模块校验和，期间新适配模块仍应标为未发布。失败停在该阶段，不删除或覆盖已经推送的标签。
- 正常 push 如被拒绝，先读远端变化，不强推、不重写历史、不更改认证或禁用 hook。
- 已推送 commit 的撤销通过后续经授权的 revert；已公开 Go 模块标签不可回收，发布错误用新 PATCH 修正。
- 发布前尚未对外的文档变动可用对应 diff 回退；不使用 reset --hard、clean，不覆盖其他工作区改动。

## R1 实际执行记录

- 已读取规则、原实施计划与旧失败记录，已核对本地/远端 main、现有标签和版本常量。
- R1 阶段未执行测试、构建、vet、lint、modern、race、覆盖率、benchmark、安全扫描或 OpenSpec 操作；后续获准检查的实际结果见 R2。
- `git diff --cached --check` 无输出，退出 0；提交清单为 48 个文件，含已有工作区依赖与锁文件更新。未修改旧 persist 实现及旧 Ristretto adapter.go。
- `git commit` 成功，提交为 `bca70090c7ad68a72e8c188c7e1deb2a8e8b9d75`，标题为“feat(cache): 添加按需引入的本地缓存适配器”。
- `git push gtkit main` 退出 0；随后 `git ls-remote --heads gtkit main` 确认远端指向上述 SHA，工作区干净。
- GitHub API 查询上述提交的 check-runs 仍返回 0，没有可据此认定 CI 通过的记录。
- 用户已选择“补跑剩余发布检查，仅豁免两个已记录的旧失败；其余通过后发版”。本次可继续 R2–R5，任何新增失败均不在豁免范围。
- R1 阶段尚未创建本轮标签或 GitHub Release；当时 README 与 CHANGELOG 继续明确标为未发布。

## R2 获准的执行明细

1. 使用已核实的本机 Go 1.27.3、golangci-lint 2.15.0、gofumpt 0.12.0、gosec 2.29.0、已有 apidiff 和 M5 使用的 govulncheck 1.9.0，不安装或更新工具，不使用容器。
2. 主模块补跑全范围 build、vet、lint、modern、race 与 atomic 覆盖率；仅用 Go 的 `-skip` 精确排除 `TestTwoLevelStoreBackfillDoesNotResurrectMutatedKey/(Delete|DeletePattern)_之后不复活`，保留同组另外两个子用例。例外原样记录，不删除测试或削弱断言。
3. 主模块及三个子模块补齐 tidy-diff、模块校验、gofumpt、gosec、benchmark 和依赖解析；主模块完整范围补跑 govulncheck。三个子模块 M5 已通过的普通测试、build、vet、lint、modern、race 与覆盖率结果可复用，正式发布依赖解析后仍需以最终模块文件作安装/构建核对。
4. 子模块发布前使用临时 modfile 指向本地主模块；发布后改用真实 v1.4.0 解析并补齐正式 go.sum。临时 replace 仅存在于项目外的证据目录，不进入正式 go.mod。
5. 对主模块和 persist 包与 v1.3.1、Ristretto 与其 v1.1.0 标签进行 apidiff；基线在临时目录导出，禁止改动既有标签。新模块无旧 API 基线。
6. 从主 README 和三个子模块 README 提取完整示例，在项目外的临时调用方模块中构建并运行独立/共享实例场景；示例使用 httptest 同步请求，不监听端口、不连接业务服务。所有日志、命令、退出码、豁免用例和覆盖率口径均保存到临时证据目录并在本文件记录摘要。
7. 任何新增失败先记录错误，区分本次改动、旧问题或工具环境问题；需要源码修正时先补齐具体计划再改。新增问题未处理或未获例外时不发版。

### R2 首轮结果与最小修正计划

证据目录：`/var/folders/_w/xwv4k3t12vn9054592gsk5200000gn/T/gincache-release-20261023-w970dfl5`。

- 主模块 build/vet/lint/modern/gosec/govulncheck、排除两个指定旧子用例的 race、benchmark 均 exit 0。覆盖率工具显示整体 80.0%；扫描无可达漏洞，仍提示一个仅依赖模块层面的公告。
- 四个模块 `go mod verify` 和三个适配器 gosec 均 exit 0。六份 README 组合程序已构建成功，运行场景待执行。
- `go mod tidy -diff` 发现主模块 go.sum 有旧版本残留并缺少当前依赖测试所需的 yaml 校验和；三个子模块 go.sum 的 testify 版本排序不符合 Go 规范，Ristretto 另有旧主模块校验和。四个 go.mod 的依赖版本无变更需求。分别记录 ERR-20261023-108/110/111/112。
- gofumpt 报告的 10 个文件均来自本次适配实现：BigCache 的 example_owner_test.go、namespace.go、owner_test.go；FreeCache 的 namespace.go；internal/adaptertest/contract.go；internal/localadapter/store.go、store_test.go；Ristretto 的 example_owner_test.go、namespace.go、owner_test.go。差异仅为导入分组、声明分组、换行与尾逗号，未发现行为差异，记录 ERR-20261023-109。
- apidiff 的 persist 和 Ristretto 比较无不兼容项；主包唯一输出为 Version 值从旧标签内的 v1.3.0 变为计划的 v1.4.0。此项是明确授权的版本元数据更新，不是函数或类型契约破坏；需逐字核对该唯一输出，不忽略其他差异，记录 ERR-20261023-113。

修正步骤（不改接口或运行行为）：
1. 先保存上述 10 个源码文件及四个模块文件的原始哈希与内容；仅对已列明文件运行既有格式化入口，回读实际 diff，出现清单外变化即停止。
2. 主模块按实际 tidy 结果整理 go.sum；子模块使用临时 modfile 整理校验和再同步 go.sum。逐个确认 go.mod 和所选依赖版本未变，正式 go.mod 不添加 replace。主模块实际发布后，再补齐其真实 v1.4.0 校验和。
3. 复验 gofumpt、四模块 tidy/verify，以及受格式化影响的内核/适配模块测试、build、vet、lint、modern、race 与覆盖率；执行剩余示例和子模块 benchmark。新输出继续记录，任何新增语义变更或失败阻断发布。
4. 这些修正均可用保存的原始文件逐个恢复；不碰两个旧用例、历史适配器或其他业务代码，不修改 OpenSpec。

复验补充：普通 tidy 未重排 BigCache/FreeCache 已有的 testify 校验和，`tidy -diff` 仍给出同一排序差异（ERR-20261023-114/115）。仅按该已读取 diff 将已有七行按 v1.3.0、v1.7.1、v1.8.0、v1.8.4、v1.10.0、v1.12.1 顺序排列，哈希原文不变；同步临时 modfile 对应的 sum，再复验 tidy-diff。此项不新增、删除或升级依赖。

### R2 最终结果

- 主模块全范围 build/vet/lint/modern/gosec/govulncheck、race 与 benchmark 已通过；race 仅跳过明确授权的两个旧子用例。主模块总覆盖率为 80.0%（go tool cover 的汇总口径），公共内核为 81.3%。
- 格式修正后，内核及三个适配模块再次通过 build/vet/lint/modern/race；BigCache/FreeCache/Ristretto 覆盖率分别为 92.1%/94.3%/85.7%。各模块 benchmark 命令 exit 0；适配模块本身没有 Benchmark 函数，不据此声称其原生引擎性能已测定。
- 四模块 tidy-diff 和 verify 通过（子模块此时仍使用项目外的本地 modfile），全仓 gofumpt 无输出；四份 go.mod 未变，不新增原生依赖或升级版本。
- 主 README 的三种组合与三个子 README 的单文件程序均构建成功，共 12 个独立/共享场景运行成功；每个场景均输出隔离的业务/HTTP 值，handler_calls=1。
- API 比较已逐条审阅：除已授权 Version 常量变化外，主包、persist 和旧 Ristretto 契约无不兼容输出。
- 三个子模块覆盖率报告初次在主模块 cwd 读取失败（ERR-116/117/118），已在正确子模块目录和对应 modfile 下重新取得；不影响先前实际测试结果。ERR-108 至 ERR-118 已按各自真实结果关闭，ERR-098 继续保留。
- 证据包含 root-checks-results.json、final-affected-checks-results.json、final-records-results.json、module-coverage-records-results.json、example-runs-results.json、api-review.json 与 release-check-summary.json。首次失败记录不删除，最终结果以上述明确复验为准。
- 仓库未跟踪 GitHub Actions 工作流，check-runs 查询无记录；没有可等待的已配置 CI，不宣称远端 CI 通过。按现有根 Makefile 的本地门禁口径和用户明确的两个旧失败例外继续发布；不直接调用会再次运行未豁免旧测试的聚合 tag 目标。

### R3 主模块标签与依赖下载进展

- 发布准备提交 `1b8c39fdc7f078f57431f330a3aef497160a8d58` 已推送；`make preflight VERSION=v1.4.0` 通过。
- 已创建并推送附注标签 v1.4.0。标签对象为 `496da6981b9ee271019d09edccf593ec285c0e39`，解引用到上述发布准备提交；Git ls-remote 和 GitHub API 均已核对。
- 首次使用公共代理连接超时（ERR-119）；恢复本机代理后 Git HTTPS 回源等待，终止该次下载并仅为后续命令使用已验证的 GitHub SSH 传输（ERR-120）。Go 已读取新标签并确认相同 Origin.Hash，但镜像校验和查询返回 unknown revision（ERR-121）。
- 官方 sum.golang.org 及其中国别名连接超时（ERR-122/123）；本机 goproxy.cn 和另一个镜像查询仍返回相同新标签不可用。保持 sumdb 校验，没有关闭验证或改用户全局配置，也没有重打已发布标签。
- 官方服务文档 https://proxy.golang.org/ 说明：在标签发布前请求过版本时，负缓存最长可能需要 30 分钟刷新。当时先等待镜像识别新版本，未跳过实际模块下载和校验。

随后镜像已返回成功。`published-root-download-final` 在保持 sumdb 校验的情况下 exit 0，Origin.Hash 与标签提交相同；主模块 zip 与 go.mod 的 h1 已取得。发布归档包含共享内核并排除三个嵌套模块，其关键文件逐项与标签内容一致。ERR-119 至 ERR-123 已关闭；等待脚本状态解析错误 ERR-124 也已修正并关闭。

### R4 实际依赖复核

三个正式 go.sum 各增加真实主模块 v1.4.0 的两条校验和，go.mod 未变。关闭本地 replace 后，各模块重新通过 tidy-diff、verify、build 和普通测试；go list 显示依赖来自缓存中的正式 v1.4.0，各模块依赖图仅含对应原生引擎。证据为 published-dependencies-results.json、published-dependency-boundaries.json。三个适配器的源码与已通过 race 的版本相同，当前仅补齐发布校验和。

提交 `1c497b7fb02f32ec7fdfe5ae4f74fe549604e779` 包含上述锁文件补齐及发布记录。三个子模块标签与 main 已原子推送；每个标签均为附注标签，解引用到该提交。主模块标签仍指向 `1b8c39fdc7f078f57431f330a3aef497160a8d58`，未覆盖已推送标签。

### R5 发布结果

| 模块 | 已推送标签 | 安装及运行 |
|---|---|---|
| 主模块 | v1.4.0 | 实际下载、sumdb 校验和归档内容核对通过 |
| BigCache | persist/bigcacheadapter/v0.1.0 | 无 replace 的 go get、build、独立/共享示例均通过 |
| FreeCache | persist/freecacheadapter/v0.1.0 | 无 replace 的 go get、build、独立/共享示例均通过 |
| Ristretto | persist/ristrettoadapter/v1.2.0 | 无 replace 的 go get、build、独立/共享示例均通过 |

- 发布版本在三个全新调用方模块中各自安装。每个依赖图只包含所选原生引擎，两个 namespace 返回各自的值，HTTP 示例每次运行的 handler_calls 均为 1。
- 四个远端标签对象类型及解引用提交已逐个核对；三个适配包的归档关键文件与各自标签内容一致。
- 发布后证据包括 published-tags.json、adapter-artifact-inspection.json、release-installs-results.json、installed-builds-results.json、installed-runs-results.json、installed-release-proof.json。
- 本次按项目的 Git 标签流程发布 Go 模块，没有另行创建 GitHub Release 页面。
- 两个旧测试失败仍记在 ERR-20261023-098，未改写源码或测试、未扩大豁免范围。发布期间发现的格式、锁文件和工具环境问题均已处置；未修改 gin_api、OpenSpec、全局工具或用户配置。
