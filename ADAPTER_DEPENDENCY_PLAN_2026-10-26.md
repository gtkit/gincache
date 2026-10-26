# 适配模块 gincache 依赖更新计划

日期：2026-10-26。当前状态：依赖与文档更新、四次 tidy、普通验证、OpenSpec 严格校验和归档均已完成；最终结果已备妥，受控检查保持未授权。

本文件记录依赖更新阶段的执行结果；后续统一版本发布及新增检查授权见 `ADAPTER_RELEASE_PLAN_2026-10-26.md`，下文未执行项均指本阶段结束时的状态。

## 需求、目标与授权

用户先要求核对三个适配模块的 gincache 版本，随后明确授权：“那就更新，并在各个模块执行一下 go mod tidy”。本次将三个适配模块的主模块最低依赖统一为已推送的 `github.com/gtkit/gincache v1.4.1`，并在根模块与三个适配模块分别执行 tidy。

授权覆盖下列 P1–P3 的依赖更新、配套说明、普通验证和本次 OpenSpec 收尾。lint、脚本 lint、vet、modern、全量 race 和覆盖率门禁仍需全部功能完成后另行授权，本计划不视作授权。不得调用包含这些检查的 Makefile 或聚合脚本。

非目标：修改 Go 源码、公共 API、缓存数据格式、存储行为、目录或依赖方向；升级原生引擎或其它依赖；处理既有失败；修改旧计划与旧 OpenSpec change；提交、打 tag、发布、推送；安装工具或运行容器。

## 已核实事实

- 主模块 `github.com/gtkit/gincache` 与三个适配模块均声明 `go 1.27`；本机 `/usr/local/bin/go` 为 `go1.27.3 darwin/amd64`。
- 远端最新主模块正式 tag 为 `v1.4.1`，对应提交 `3cbd8cf17a087dfc64799a59bbc963ad9bb5127a`，已用 `git ls-remote --tags gtkit` 核对。
- BigCache、FreeCache 目前要求 gincache v1.4.0；Ristretto 工作区已要求 v1.4.1，且其 go.mod/go.sum 是本次开始前已有修改，必须保留。
- 原生依赖保持 BigCache v3.2.0、FreeCache v1.2.7、Ristretto v2.4.2；JSON 仍为 gtkit/json v1.0.0，不新增依赖。
- 根目录与三个模块 README、go.work.example 中仍有 v1.4.0 的当前使用示例。已发布适配器 tag 不会因工作区修改而改变；安装示例需要显式选择 gincache v1.4.1。
- 当前 `go env GOWORK` 为空；正式 go.mod 不含 replace。执行时显式设置 `GOWORK=off GOTOOLCHAIN=local GOFLAGS=''`，防止 workspace 或默认 flags 掩盖实际依赖。
- 本仓 PATH 中无 openspec 命令；已有 `/Users/xiaozhaofu/go/src/Project/gin_api/.openspec-auto/tools/run_openspec.py` 可在本仓 cwd 调用已安装 CLI，`list --json` 已成功。现存 active change 均不匹配本次依赖更新。
- 已读取全局/项目规则、相关 Guide、既有适配器与发布计划和错误记录。`ERR-20261023-098` 记载主模块两个既有删除场景失败，本次不重跑或修改这两个场景；不将历史验证当作本轮结果。

## 实施范围与兼容约束

1. 根目录 `go.mod`、`go.sum`：仅接受 tidy 必要结果，不手动升级版本。
2. `persist/bigcacheadapter/go.mod`、`go.sum`：gincache 最低版本 v1.4.0 → v1.4.1，并 tidy。
3. `persist/freecacheadapter/go.mod`、`go.sum`：同上。
4. `persist/ristrettoadapter/go.mod`、`go.sum`：保留已有 v1.4.1 修改，再 tidy。
5. `README.md` 与三个适配模块 `README.md`：同步当前依赖和本地 replace 示例；发布安装命令显式选择主模块 v1.4.1。明确既有示例运行证据来自先前发布检查，不能声称新组合已运行。
6. 根目录与三个适配模块 `CHANGELOG.md`：仅在 Unreleased 记录本次最低依赖更新，不改历史版本内容。
7. `go.work.example`：版本限定的 gincache replace 同步到 v1.4.1；不创建自动生效的 go.work。
8. 本计划及独立 OpenSpec change `upgrade-adapter-gincache-dependency`：记录方案、任务、事实和本次执行结果，完成后按 CLI 实际输出归档。

不改各模块的 module path、Go 版本、版本常量或自身发布 tag；不添加永久 replace。API、旧 Ristretto 入口、TTL、namespace、资源所有权、存储格式和依赖隔离约束保持。v1.4.1 已记录的修复是最终响应状态码和空策略缓存键处理，本次仅选择该已有版本；运行兼容性在本轮验证前保持待验证。

## 分阶段步骤与验收

| 阶段/步骤 | 前置条件 | 具体动作 | 检查方法和证据 | 停止条件 | 交付物与状态 |
|---|---|---|---|---|---|
| P1.1 | 本计划落盘且回读，规则/错误记录已读 | 新建独立 OpenSpec change，按 CLI instructions 完成 proposal、design、specs、tasks | status JSON 的 applyRequires 全部 done；回读四类产物 | 无匹配 CLI、任务冲突或 apply 未就绪 | OpenSpec 产物；已完成 |
| P1.2 | 目标文件范围确认 | 将所有目标文件当前内容和初始 Git diff 备份到系统临时目录，记录文件哈希与路径 | 文件清单、哈希及备份路径 | 文件出现未评估的并行修改 | 可逐文件回滚的备份；已完成 |
| P2.1 | P1 完成且已重读原计划 | 仅提升两个旧依赖声明，保留 Ristretto 既有修改；同步上述 README/CHANGELOG/workspace 示例 | 回读及精确 diff：目标版本一致，无其它版本升级或源码修改 | 需要更改公共 API、模块版本或架构 | 依赖和说明改动；已完成 |
| P2.2 | 版本声明完成 | 依次在根、BigCache、FreeCache、Ristretto 目录运行 `GOWORK=off GOTOOLCHAIN=local GOFLAGS='' /usr/local/bin/go mod tidy` | 各目录命令、退出码、输出及 tidy 前后 diff | 下载/校验失败、额外版本升级、用户要求停止 | 四次真实 tidy 结果；已完成 |
| P3.1 | 全部更新和 tidy 完成 | 根模块执行 `go build -mod=readonly ./...`；三个适配模块分别执行 `go test -mod=readonly -vet=off -count=1 -timeout=120s ./...`，沿用上述环境 | 独立日志、真实退出码；测试不隐式运行 vet | 新失败先记录，不修改范围外源码、不重跑主模块旧失败 | 构建/普通测试证据；已完成 |
| P3.2 | P3.1 已记录，失败未伪报通过 | 回读目标文件和最终 diff；严格校验本次 OpenSpec change，任务完成后归档并核实目录移动及目标 spec | validate JSON、archive 输出、归档目录与目标 spec 的实际状态 | 校验或归档未完成；不能仅凭退出 0 判为归档成功 | 最终 diff、严格校验和归档；已完成 |
| P3.3 | 授权范围内工作已完成 | 汇报更新和各模块 tidy/普通验证结果；单独询问是否运行受控检查 | 明确已执行、未执行与历史限制 | 未获授权不运行受控检查、不提交或发布 | 最终说明已备妥；受控检查尚未授权 |

## 风险、失败处理与回滚

- tidy 可能重排直接/间接依赖和清理无用校验和，必须逐项读 diff；不使用 `go get -u` 或未经核对的最新依赖。
- 代理或校验和失败保留实际输出，不关闭 sumdb/TLS、不安装工具、不以本地 replace 掩盖发布依赖。对失败先追加错误记录，再在本范围内处理或报告。
- 发生并行修改时停止受影响文件写入，比较备份与现状；不得覆盖用户修改。
- 回滚只反向撤销本次 hunk，或在确认没有并行修改时使用本次备份；Ristretto 必须恢复到任务开始时已更新 v1.4.1 的状态，而非 Git HEAD。不执行 reset、clean 或改写历史。
- 未提交、未发布，因此无需下游数据迁移。代码与行为测试结果不能被 tidy 成功替代；未执行项分别说明。

## 执行记录

- P1.2 已完成：17 个目标文件和初始 Git diff 已备份到 `/var/folders/_w/xwv4k3t12vn9054592gsk5200000gn/T/gincache-deps-20261026-oaz1gkhk`。`manifest.json` 记录逐文件 SHA-256；`before/` 保存任务开始时内容，`initial.diff` 保存既有修改。备份命令 exit 0。
- P1.1 已完成：独立 change 的四类产物已回读；status JSON 中 applyRequires 为 tasks 且全部产物 done，apply instructions 为 ready。相关命令均 exit 0。
- P2.1 已完成：两个旧依赖声明已更新，Ristretto 原声明保留；四份 README/CHANGELOG 和 go.work.example 已同步并回读 diff。已发布适配器版本号不变，安装示例显式要求主模块 v1.4.1。
- P2.2 已完成：四次 tidy 均 exit 0，均无错误输出；根模块与 Ristretto 的依赖文件未再变化，BigCache/FreeCache 仅更新 gincache v1.4.1 的 go.sum 条目。完整命令、目录、环境、耗时和输出见备份目录下 `tidy-results.json` 及 `*-tidy.log`。已回读全部依赖 diff，无其它依赖版本变更。
- P3.1 已完成：本机 Go 1.27.3，在同一 `GOWORK=off GOTOOLCHAIN=local GOFLAGS=''` 环境下，根模块 `go build -mod=readonly ./...` exit 0；BigCache、FreeCache、Ristretto 的 `go test -mod=readonly -vet=off -count=1 -timeout=120s ./...` 均 exit 0，测试输出耗时分别为 1.148s、1.134s、0.998s。日志及完整命令见 `verification-results.json`、`root-build.log` 与三个 `*-test.log`。
- P3.2 已完成：最终依赖差异已回读；根模块和 Ristretto 的 go.mod/go.sum 与任务开始时逐字节一致，另两个模块只有 gincache 声明/校验和变化；三个模块的 v1.4.1 校验和一致，无 Go 源码修改。证据为 `final-dependency-review.json`。本次 OpenSpec 严格校验 exit 0，1 项通过、0 项失败、issues 为空。
- OpenSpec 归档命令 exit 0，实际输出为新增 1 项要求、规格更新成功，归档名称 `2026-10-26-upgrade-adapter-gincache-dependency`。已回读目标规格及归档 tasks，9 个任务全部完成；active 目录不存在、archive 目录存在。随后 `validate optional-local-adapters --type spec --strict --json --no-interactive` exit 0，1 项通过、0 项失败、issues 为空。旧 active change 未处理。
- 未执行：vet、lint、脚本 lint、modern、全量 race、覆盖率、benchmark、安全扫描和主模块全量测试；未运行 Docker、未提交、未打 tag、未发布或推送。既有失败未重跑，README 完整示例没有在本轮单独构建运行。

## 合规检查摘要

- 实际目标为项目已有 Go 1.27，验证使用本机 Go 1.27.3；未改变语言版本。
- 仅升级已存在的 gincache 最低依赖；未新增依赖或改动其他依赖版本。
- 无 Go 源码、公开 API、Option、默认值、错误体系、并发契约或存储格式变更。
- 四个模块 tidy、根模块构建和三个适配模块普通测试均有本轮 exit 0 证据；覆盖率及其他未授权门禁未宣称通过。
- README、安装命令、workspace 示例和 Unreleased 记录已同步；Ristretto 既有修改完整保留。
