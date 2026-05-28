# Changelog

遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 与 [Semantic Versioning](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### Added
### Changed
### Deprecated
### Removed
### Fixed
### Security

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
