## Why

项目约束明确禁止库代码直接使用 `encoding/json`。当前根模块仍有多个源码和测试文件直接导入 `encoding/json`，会让 JSON 行为绕过 `github.com/gtkit/json` 的统一入口。

## What Changes

- 将项目内直接使用 `encoding/json` 的缓存序列化和测试代码改为使用 `github.com/gtkit/json`。
- 为根模块增加导入策略回归测试，防止后续源码重新直接导入 `encoding/json`。
- 根模块显式依赖 `github.com/gtkit/json`，版本与 `persist/ristrettoadapter` 子模块保持一致。

## Capabilities

### New Capabilities

- `json-serialization-policy`: 约束项目内 JSON 编解码必须通过 `github.com/gtkit/json`，并禁止项目源码直接导入 `encoding/json`。

### Modified Capabilities

无。

## Impact

- 影响 `cache.go`、`persist` 包和相关测试中的 JSON import。
- 根模块 `go.mod`/`go.sum` 将增加 `github.com/gtkit/json` 直接依赖及其必要传递依赖记录。
- 不新增导出 API，不改变缓存数据结构和 JSON wire format。
