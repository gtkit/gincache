# json-serialization-policy Specification

## Purpose
约束项目内 JSON 编解码统一走 `github.com/gtkit/json`，避免库代码直接依赖标准库 `encoding/json`。
## Requirements
### Requirement: JSON operations use gtkit json package
Project JSON serialization and deserialization code SHALL use `github.com/gtkit/json` instead of importing `encoding/json` directly.

#### Scenario: Cache serialization uses gtkit json
- **WHEN** cache response, memory store, Redis store, TwoLevelStore, or Ristretto adapter code marshals or unmarshals JSON
- **THEN** the code MUST call JSON APIs through `github.com/gtkit/json`

### Requirement: Direct encoding/json imports are rejected
Project Go source files SHALL NOT directly import `encoding/json`.

#### Scenario: Import policy regression test runs
- **WHEN** `go test ./...` runs in the root module
- **THEN** the test suite MUST fail if a project Go source file directly imports `encoding/json`

#### Scenario: Ristretto adapter import policy is checked
- **WHEN** `go test ./...` runs in the `persist/ristrettoadapter` module
- **THEN** the test suite MUST fail if a Ristretto adapter Go source file directly imports `encoding/json`
