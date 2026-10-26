# 三个适配模块统一版本发布计划

日期：2026-10-26。当前状态：三个模块已分别提交，三个 v1.2.1 附注标签及 main 已成功原子推送至 GitHub；远端引用、实际安装、发布内容及构建均已核对通过。

## 来源、目标与授权边界

用户在依赖更新完成后要求：“分别提交，发版，并 push 到 GitHub，最好能让这三个小模块都是同样的 tag”。本次继续发布已完成的适配模块依赖更新，不恢复其他旧任务。

目标：BigCache、FreeCache、Ristretto 分别提交，统一模块版本为 `v1.2.1`，创建三个带路径前缀的附注标签并推送到远端 `gtkit`。共用文档单独提交；根模块不另行发版。

提交、附注标签和正常推送已获本次明确授权，不重复索要。全局规则规定发布授权不包含 lint、vet、modern、全量 race 和覆盖率门禁；先完成全部版本与文档准备，再请求本次发布检查授权，获准并通过后才提交和发布。不运行会隐式触发未授权检查的 Makefile 或 hook。

非目标：新增功能、修改缓存实现/接口/数据格式、调整目录或依赖方向、升级其他依赖、变更根模块版本、修复旧失败、安装工具、创建容器、改写已推送历史或旧标签。沿用项目已有 Go 模块标签发布方式，不额外创建 GitHub Release 页面。

## 已核对事实

- 分支为 `main`；本地 HEAD 与远端 main 均为 `3cbd8cf17a087dfc64799a59bbc963ad9bb5127a`；远端为 `git@github.com:gtkit/gincache.git`。
- 主模块最新发布为 `v1.4.1`。三个适配器当前正式版本分别为 BigCache v0.1.0、FreeCache v0.1.0、Ristretto v1.2.0。远端没有本次三个 v1.2.1 标签。
- 本次统一的是语义版本部分；Git 标签完整名称必须包含各自模块目录，不能用一个根目录标签同时发布三个嵌套模块。
- 依赖更新已完成：三个 go.mod 均要求 gincache v1.4.1；根模块及三个子模块 tidy、根构建和三个子模块普通测试通过。真实记录见 `ADAPTER_DEPENDENCY_PLAN_2026-10-26.md`，该轮未运行受控门禁。
- 当前未提交差异为上述依赖和配套 README/CHANGELOG/workspace 示例，另有上一轮实施记录；没有无关 Go 源码变化。Ristretto 最初已有的依赖更新仍保留，本次发布包含它。
- 本机 Go 为 1.27.3；已安装 golangci-lint 2.15.0、gofumpt 0.12.0、govulncheck 1.8.0、gosec 2.29.0。以实际本机工具为准，不沿用旧计划中的工具版本。
- 根 Makefile 提供发布门禁；Ristretto Makefile 是通用模板，直接调用会生成不带目录前缀的标签并自动改版本，因此本次按已读命令逐项执行，不调用其发版目标。
- 仓库未跟踪 `.github` 工作流。没有据此认定远端 CI 通过；推送前后仍核对真实远端引用。
- 根模块的两个既有删除用例失败由 `ERR-20261023-098` 记录；本次仅发布三个子模块，不运行根模块全量测试或扩大修复范围。
- 本轮误读不存在的发布脚本已记录为 `ERR-20261026-129` 并关闭；已核对实际 Makefile，不再猜测脚本路径。

## 版本与兼容方案

| 模块 | 旧版本 | 目标版本 | 完整标签 |
|---|---|---|---|
| BigCache | v0.1.0 | v1.2.1 | persist/bigcacheadapter/v1.2.1 |
| FreeCache | v0.1.0 | v1.2.1 | persist/freecacheadapter/v1.2.1 |
| Ristretto | v1.2.0 | v1.2.1 | persist/ristrettoadapter/v1.2.1 |

v1.2.1 是高于三个当前版本的最小共同补丁版本。BigCache、FreeCache 随本次统一进入 v1 稳定版本线，后续遵守 v1 API 兼容承诺；本次没有增加或删除导出 API。Ristretto 为补丁升级。Go 仍为 1.27，gincache 依赖仍为 v1.4.1，三个原生引擎版本及 JSON 依赖不变，不引入 v2。

元数据文件：三个子模块各自 `version.go`、`README.md`、`CHANGELOG.md`；提交时同时包含对应 `go.mod`、`go.sum` 的现有依赖更新。每份 CHANGELOG 保留空 Unreleased，并将本次条目归入 `[1.2.1] - 2026-10-26`；README 版本及安装/本地替换示例同步。根 README 的适配器版本、根 Unreleased 描述同步，保留根 `Version = "v1.4.1"`。

## 阶段与验收

| 阶段 | 前置条件 | 步骤与交付物 | 检查方法及证据 | 停止条件 | 状态 |
|---|---|---|---|---|---|
| R1 准备 | 已读规则、错误记录、依赖计划与远端；本计划保存回读 | 备份本轮目标文件；更新三个 Version、发布条目及四份 README；准备三份中文标签说明 | 回读 diff、版本/模块路径/日期一致；准备文件清单与快照 | 出现无关修改、目标标签已存在、需要 API 或结构变更 | 已完成 |
| R2 授权 | 全部 R1 准备完成 | 提交具体的发布检查范围供用户单独授权 | 用户本轮明确答复；未授权项保持未执行 | 未获准受控门禁时不得提交/打标签 | 已完成，用户要求立即继续 |
| R3 检查 | R2 明确获准 | 在三个子模块分别运行下列门禁，按各模块旧标签核对 Go 源码差异只含 Version；记录日志和退出码 | 所有获准门禁通过，覆盖率达 80%；失败日志保留 | 新失败先记录；需要范围外源码修正、安装或例外时停止依赖步骤 | 已完成 |
| R4 分别提交 | R3 通过；逐文件回读；核对 hooks 不扩展授权；远端 main 未前移 | 分别提交 BigCache、FreeCache、Ristretto 的五个文件，再单独提交根 README/CHANGELOG/go.work.example 与两个本轮计划；不使用 git add . | 四个中文 commit、各自文件列表、工作区干净 | 暂存无关文件、hooks 拒绝、检查失效或远端分叉 | 已完成 |
| R5 发布与推送 | R4 完成；同名标签不存在；三个模块元数据一致 | 在最终共享提交创建三个附注标签，使用三份独立中文说明；以一次正常原子 push 推送 main 和三个标签 | git cat-file 验证 tag 类型；本地解引用与 ls-remote 精确一致 | 非快进、认证失败、原子推送失败；禁止强推或覆盖标签 | 已完成 |
| R6 发布核对 | 已推送标签可确认 | 在项目外临时目录分别解析发布模块，读取真实版本与 gincache v1.4.1 依赖，核对可安装性；将实际结果单独提交并推送为发布记录，不改动已推送标签 | Go 下载/校验、Origin 或标签提交、版本常量、最终远端状态 | 代理或网络未就绪时如实报告，不关闭 TLS/sumdb、不伪报完成 | 安装、比对、构建已完成；最终记录随本次提交同步 |

### R3 本次执行的具体检查

环境统一为 `GOWORK=off GOTOOLCHAIN=local GOFLAGS=''`，工作目录分别是三个子模块。结果写入项目外证据目录。

1. `go mod tidy -diff`、`go mod verify`、`go build -mod=readonly ./...`。
2. `go vet ./...`、`golangci-lint run ./...`、`go fix -diff ./...`、`gofumpt -l .`；仅只读检查，不自动应用格式化或 modern 改写。
3. `go test -mod=readonly -vet=off -race -covermode=atomic -coverprofile=<证据目录/模块.cover> -count=1 -timeout=5m ./...`，读取真实总覆盖率并要求 ≥80%。
4. `go test -mod=readonly -vet=off -run='^$' -bench=. -benchmem -count=3 ./...`；若没有 Benchmark 函数，只记录该事实，不宣称性能数据。
5. `gosec -quiet ./...`、`govulncheck ./...`；保留真实告警，不自动升级依赖或扩充修复范围。
6. 按对应旧标签比较源码/导出面，仅允许已授权 Version 元数据变化；不重新运行根模块的旧失败用例。

## 失败处理、回滚与发布约束

- 写入前对本轮目标文件保存内容与哈希；回滚只撤销本次版本准备，不回退上一轮用户已经授权的依赖更新。
- 不修改原生引擎、go.mod 的 Go 版本或项目架构。新检查失败先记录原始事实；不删测试、不调低覆盖率、不自动接受历史例外。
- 不调用会自动提交/改版本的 release 脚本。提交只使用确切文件清单和中文说明，标签说明使用临时文件传递。
- 三个标签指向最后的共享文档提交，保证同一仓库快照中的版本号全部一致；三个独立模块提交仍各自可审阅。
- 推送前禁止访问未发布的目标版本代理路径，避免主动制造负缓存。推送失败先核对远端，不强推、不覆盖已经存在的标签。
- 标签公开后不删除、不重打；错误通过后续单独获准的补丁发布修正。Go 代理暂未收录时区分“GitHub 标签已发布”和“代理安装尚未验证”。
- 本次仅准备版本元数据与发布已有变更，没有新的功能契约；上一轮 OpenSpec 已归档，本轮不重开或改写它。

## 执行记录

- R1 已完成：20 个目标文件按发布准备前内容备份到 `/var/folders/_w/xwv4k3t12vn9054592gsk5200000gn/T/gincache-release-20261026-omwolq_s`；`manifest.json` 记录哈希，`initial.diff` 保留既有依赖更新。
- 三个 Version 均为 v1.2.1；三个 CHANGELOG 均含 `[1.2.1] - 2026-10-26` 且保留空 Unreleased；根 README 及三个子 README 的安装、替换和版本说明均已同步。主模块 Version 仍为 v1.4.1。
- 三个模块分别与其上一发布标签比较 `.go` 文件，唯一差异均为 Version 常量值。README 当前说明中已无旧适配模块版本引用。上述实际只读 diff 已回读；`git diff --check` exit 0。
- 三份中文附注标签说明已保存并回读，文件为证据目录下 `bigcacheadapter-tag-message.txt`、`freecacheadapter-tag-message.txt`、`ristrettoadapter-tag-message.txt`。
- Git 没有配置 `core.hooksPath`，默认 `.git/hooks` 中无可执行的非示例 hook。尚未执行 git add、commit、tag 或 push。
- R2 已完成：上一轮列明三个模块发布门禁和“通过后直接分别提交并推送标签”，用户答复“那就发啊，你等啥啊”。按该答复继续既定 R3–R6，不重复询问发布许可。此前未执行的检查从本轮开始记录真实结果。
- R3 已完成：三个模块各 12 项、共 36 项实际命令全部 exit 0；tidy-diff、模块校验、build、vet、lint、modern、gofumpt、race/coverage、覆盖率汇总、benchmark 入口、gosec 和 govulncheck 均通过。modern/gofumpt 无输出，govulncheck 均输出 `No vulnerabilities found.`。命令、环境、退出码、耗时及日志路径保存为证据目录的 `release-checks.json`。
- 真实语句覆盖率：BigCache 92.1%、FreeCache 94.3%、Ristretto 85.7%，均达到 80% 门槛。benchmark 入口成功但未输出基准条目，不据此声称测得性能数据。本轮没有运行根模块全量测试或修改既有失败。
- 提交前已准备逐模块文件清单及哈希：证据目录 `commit-plan.json`。每个适配器只提交自己的 go.mod/go.sum/version.go/README.md/CHANGELOG.md；共用说明另行提交。
- R4 的三个模块提交已完成：BigCache `b69e2c4cb85d098ef3250970f988517f3d1b9783`、FreeCache `0b334d65354d7d03c0826464759721f1a2300940`、Ristretto `58a8c781467505cbd8071b022774554c385b3840`。每个提交均精确包含对应模块的五个文件，提交前空白检查通过；共用文档和远端推送结果见下方记录。
- R4 共用文档提交已完成：`37604f9f34036b662dbf536cb0faf0ed2f0c8d55`，标题为“docs(cache): 同步三个适配模块的统一版本与发布记录”，包含根 README/CHANGELOG/go.work.example 和两个本轮计划；提交后工作区干净。
- R5 已完成：创建三个附注标签，均指向 `37604f9f34036b662dbf536cb0faf0ed2f0c8d55`，并与 main 一次原子推送成功。BigCache 标签对象为 `e3e9d225c804211fb254b9c783cebb8fc73f9800`，FreeCache 为 `9ba20400068d07fefa3053d54b0bdbb0e0bb1b6b`，Ristretto 为 `c1b5d65c25de4b82e3dc0cc606090c3ec39b63b7`。远端对象和解引用 SHA 逐项匹配，证据见 `tags.json`、`tag-push-commands.json`、`published-refs.json`。
- R6 首轮：BigCache 的 go get、go list、go mod download 均 exit 0，实际解析为 v1.2.1 和 gincache v1.4.1。临时核验器误将代理未返回的 Origin.Hash 视为必须字段而退出 1，记录 ERR-20261026-131。修正核验方式为：保留成功下载结果，逐文件比对已下载模块的 Go 源码和元数据与已发布提交，完成剩余模块解析；不更改源码、标签或校验安全设置。
- R6 最终结果：三个独立临时调用方均成功安装对应模块 v1.2.1，主模块实际选择 v1.4.1；下载校验和已取得。BigCache/FreeCache/Ristretto 分别有 9/9/12 个 Go 源码及模块、说明文件与已发布提交逐字节一致，三个安装版本的外部调用方构建均 exit 0。证据为 `final-installation-commands.json`、`installed-release-proof.json`；安装目录为 `/var/folders/_w/xwv4k3t12vn9054592gsk5200000gn/T/gincache-release-install-slx08cfu`。ERR-131 已关闭，原失败记录保留。
- 本次按现有 Go 模块方式发布三个带目录前缀的 Git 标签；未另建 GitHub Release 页面，未变更根模块 v1.4.1 标签，也未覆盖任何旧标签。最终发布记录作为单独 docs 提交同步，三个发布标签保持原提交指向。

## 合规检查摘要

- 三个模块各有独立中文提交，版本常量/CHANGELOG/README/标签的版本部分统一为 v1.2.1。
- 发布检查全部通过，覆盖率均超过 80%；实际安装和构建有独立调用方证据。
- Go 版本、module path、依赖方向和既有 API 保持；未新增依赖，gincache 最低依赖为 v1.4.1。
- 未运行 Docker，未重跑根模块旧失败，未声称远端 CI 或未创建的 Release 页面通过/存在。
