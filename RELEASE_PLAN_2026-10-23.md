# 可选缓存适配模块发布计划

日期：2026-10-23。状态：发布准备；尚未创建或推送本轮标签。

## 来源、范围和当前事实

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
| R1 提交与推送 | 读取规则、原计划、旧失败记录、当前 diff 和远端引用；回读本计划；按明确文件清单提交；正常推送 main | 暂存文件清单、commit SHA、push 返回及远端 main SHA | 非预期文件、并行改动、远端前移、认证或 hook 拒绝 | 待执行提交 |
| R2 发布门禁决定 | 功能已完成；受控检查须用户单独授权。先说明旧失败与尚缺证据；由用户决定补跑发布门禁及旧失败例外，或明确采用现有范围检查作为发布依据 | 用户明确指令；获准后记录真实命令、退出码和被接受的例外 | 未获得对应检查授权或发布例外；新失败未经处理 | 待确认，不自动运行 |
| R3 主模块发布 | R2 条件满足；将 CHANGELOG 的 Unreleased 归入 1.4.0 和实际发布日期，同步 README 发布状态，提交并推送；确认工作区干净及远端标签不存在后创建附注标签并正常推送 | 版本常量、CHANGELOG、标签类型及解引用 SHA、远端引用 | 门禁未满足、版本或文档不一致、已有同名远端标签 | 待执行 |
| R4 子模块发布 | 主模块 v1.4.0 已可解析；按实际下载结果补齐子模块 go.sum，不升级依赖；将三份 CHANGELOG 归入对应版本并同步 README，提交并推送；创建三个带路径前缀的附注标签 | 每个模块的真实解析结果、锁文件 diff、版本与标签对应关系、远端引用 | 主模块不可解析、依赖意外变化、标签冲突 | 待执行 |
| R5 交付核对 | 所有拟发布引用已推送；核对远端指向和安装解析结果，报告已完成与未完成事项；GitHub Release 页面与 Go 模块标签发布分开记录 | GitHub commit/tag 链接、模块解析证据；页面未创建时明确说明 | 无法核实的结果不得报告成功 | 待执行 |

发布检查按实际脚本及项目规则拆开执行，不通过聚合入口间接运行尚未授权的 vet、lint、modern、race 或覆盖率检查。
当前 README 和 CHANGELOG 保持“未发布”事实；完成对应发布阶段时再更新，不提前宣称标签或 GitHub Release 已存在。

## 风险、失败处理与回滚

- 两个旧失败与完整门禁缺口阻止按默认规则直接打标签；不能将局部测试通过改写成全仓通过。
- 主模块先发布、子模块后补齐实际模块校验和，期间新适配模块仍应标为未发布。失败停在该阶段，不删除或覆盖已经推送的标签。
- 正常 push 如被拒绝，先读远端变化，不强推、不重写历史、不更改认证或禁用 hook。
- 已推送 commit 的撤销通过后续经授权的 revert；已公开 Go 模块标签不可回收，发布错误用新 PATCH 修正。
- 发布前尚未对外的文档变动可用对应 diff 回退；不使用 reset --hard、clean，不覆盖其他工作区改动。

## 实际执行记录

- 已读取规则、原实施计划与旧失败记录，已核对本地/远端 main、现有标签和版本常量。
- 本轮尚未执行测试、构建、vet、lint、modern、race、覆盖率、benchmark、安全扫描或 OpenSpec 操作。
- 尚未 commit、push、创建标签或 GitHub Release。
