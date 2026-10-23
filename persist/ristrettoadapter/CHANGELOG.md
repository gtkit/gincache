# Changelog

## [Unreleased]

## [1.2.0] - 2026-10-23

### Added

- 新增 Owner 与隔离 namespace 视图，支持现有 gincache 存储接口、精确 TTL 与自建/借用生命周期。
- 提供按需引入、独立实例和共享实例的完整示例，说明 TTL、容量、键隔离与生命周期。

### Changed

- 原生 Ristretto 依赖为 v2.4.2，最低 Go 版本为 1.27；原有 New、Store 与 Option 用法继续保留。
