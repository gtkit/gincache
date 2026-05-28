## Context

本仓库是 Go 扩展包，项目规则要求 JSON 必须统一使用 `github.com/gtkit/json` 或 `github.com/gtkit/json/v2`，禁止直接使用标准库 `encoding/json`。当前子模块 `persist/ristrettoadapter` 已经依赖 `github.com/gtkit/json v0.2.10`，根模块尚未显式依赖该包。

## Goals / Non-Goals

**Goals:**

- 根模块和 `persist/ristrettoadapter` 子模块源码不再直接 import `encoding/json`。
- 现有 JSON Marshal、Unmarshal、RawMessage 用法保持等价调用方式。
- 增加测试约束，防止后续回退到直接 import `encoding/json`。
- 根模块与子模块使用同一个 `github.com/gtkit/json` 版本。

**Non-Goals:**

- 不替换第三方依赖内部对 `encoding/json` 的使用。
- 不调整 JSON 字段 tag、缓存格式或对外 API。
- 不升级到 `github.com/gtkit/json/v2`，避免在本次小改中引入额外迁移风险。

## Decisions

- 使用 `github.com/gtkit/json v0.2.10`：该版本是当前可查询到的最新版本，并且子模块已经使用它，根模块跟随同版本可以避免同仓库版本分叉。
- 直接替换 import alias 为 `json`：当前代码使用 `json.Marshal`、`json.Unmarshal`、`json.RawMessage`，`github.com/gtkit/json` 已在当前版本提供同名入口，替换成本最低。
- 新增 AST import 检查测试：用 `go/parser` 读取项目 Go 文件 import，不依赖字符串误判，并且能在普通 `go test ./...` 中自动执行。

## Risks / Trade-offs

- `github.com/gtkit/json` 在不同 build tag 下可能选择不同后端；通过保持同名 API 和现有测试覆盖降低行为差异风险。
- 导入策略测试需要维护扫描范围；本次会递归扫描仓库内 Go 文件，并跳过 `.git`、`openspec` 和构建缓存类目录，减少漏检。

## Migration Plan

1. 添加导入策略测试并确认它能捕获当前 `encoding/json` import。
2. 替换项目源码和测试 import 为 `github.com/gtkit/json`。
3. 运行根模块和 `persist/ristrettoadapter` 子模块的测试与 tidy 检查。
