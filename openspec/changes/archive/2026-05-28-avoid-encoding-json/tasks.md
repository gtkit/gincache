## 1. JSON import replacement

- [x] 1.1 Add a regression test that rejects direct `encoding/json` imports in project Go files.
- [x] 1.2 Replace root module `encoding/json` imports with `github.com/gtkit/json`.
- [x] 1.3 Ensure `persist/ristrettoadapter` uses `github.com/gtkit/json` and has the dependency retained by tidy.

## 2. Verification

- [x] 2.1 Run the import regression test before replacement and confirm it fails for existing direct imports.
- [x] 2.2 Run `go test ./...` in the root module.
- [x] 2.3 Run `go test ./...` in `persist/ristrettoadapter`.
- [x] 2.4 Run `go mod tidy -diff` in both modules and confirm no unexpected dependency changes remain.
