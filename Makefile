.PHONY: help fmt lint test test-submodules bench cover sec vuln tidy-check gate preflight tag

LINT_TARGETS ?= ./...
# 独立的嵌套 module，主模块的 ./... 覆盖不到，必须单独跑。
SUBMODULES ?= persist/ristrettoadapter
MIN_COVERAGE ?= 80

help: ## 显示可用目标
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

fmt: ## 格式化源码（会改写文件）
	gofumpt -l -w .

lint: ## 静态检查（只读，不改写源码）
	@set -e; \
	unformatted=$$(gofumpt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "❌ 以下文件未格式化，请先执行 make fmt："; echo "$$unformatted"; exit 1; fi
	go vet ./...
	golangci-lint run $(LINT_TARGETS)

tidy-check: ## 校验 go.mod / go.sum 已是 tidy 状态（不改写文件）
	go mod tidy -diff
	@set -e; for m in $(SUBMODULES); do \
		echo "▶️ go mod tidy -diff ($$m)"; (cd $$m && go mod tidy -diff); done

test: ## 竞态测试（主模块）
	go test -race -count=1 -timeout=5m ./...

test-submodules: ## 竞态测试（嵌套 module）
	@set -e; for m in $(SUBMODULES); do \
		echo "▶️ go test ($$m)"; (cd $$m && go vet ./... && go test -race -count=1 -timeout=5m ./...); done

bench: ## 基准测试
	go test -bench=. -benchmem -count=3 ./...

cover: ## 覆盖率，低于 MIN_COVERAGE 即失败
	@set -e; \
	go test -coverprofile=coverage.out ./...; \
	total=$$(go tool cover -func=coverage.out | awk '/^total:/ {gsub("%","",$$3); print $$3}'); \
	echo "总覆盖率 $$total%（阈值 $(MIN_COVERAGE)%）"; \
	awk -v t="$$total" -v m="$(MIN_COVERAGE)" 'BEGIN { exit (t+0 >= m+0) ? 0 : 1 }' \
		|| { echo "❌ 覆盖率低于阈值"; exit 1; }

sec: ## 安全扫描（需要 go install github.com/securego/gosec/v2/cmd/gosec@latest）
	gosec -quiet ./...
	@set -e; for m in $(SUBMODULES); do \
		echo "▶️ gosec ($$m)"; (cd $$m && gosec -quiet ./...); done

vuln: ## 漏洞扫描（需要 go install golang.org/x/vuln/cmd/govulncheck@latest）
	govulncheck $(LINT_TARGETS)
	@set -e; for m in $(SUBMODULES); do \
		echo "▶️ govulncheck ($$m)"; (cd $$m && govulncheck ./...); done

gate: lint tidy-check test test-submodules bench cover sec vuln ## 发版前完整门禁

## 发版前置检查。版本准备（改 version.go、归档 CHANGELOG）必须先单独提交，
## 这个目标只负责确认状态是否允许发版，不修改任何文件。
preflight:
	@set -e; \
	if [ -z "$(VERSION)" ]; then \
		echo "❌ 用法：make tag VERSION=v1.3.0 [MESSAGE_FILE=/path/to/msg]"; exit 1; fi; \
	echo "$(VERSION)" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+(-(rc|beta|alpha)\.[0-9]+)?$$' \
		|| { echo "❌ 版本号格式不合法：$(VERSION)（应形如 v1.3.0 或 v1.3.0-rc.1）"; exit 1; }; \
	if [ -n "$$(git status --porcelain)" ]; then \
		echo "❌ 工作区不干净，发版前必须全部提交："; git status --short; exit 1; fi; \
	branch=$$(git rev-parse --abbrev-ref HEAD); \
	if [ "$$branch" != "main" ]; then \
		echo "❌ 当前分支是 $$branch，发版必须在 main"; exit 1; fi; \
	if [ -n "$$(git ls-remote --tags gtkit "refs/tags/$(VERSION)")" ]; then \
		echo "❌ 远端已存在 tag $(VERSION)。已推送的 tag 不得重命名、删除或覆盖，请改用新版本号"; exit 1; fi; \
	if git rev-parse -q --verify "refs/tags/$(VERSION)" >/dev/null; then \
		if [ "$$(git cat-file -t "$(VERSION)")" != "tag" ]; then \
			echo "❌ 本地 tag $(VERSION) 不是附注标签，请删除后重来"; exit 1; fi; \
		if [ "$$(git rev-list -n 1 "$(VERSION)")" != "$$(git rev-parse HEAD)" ]; then \
			echo "❌ 本地 tag $(VERSION) 指向的不是当前 HEAD，请删除后重来"; exit 1; fi; \
		echo "ℹ️  本地已有指向 HEAD 的附注标签 $(VERSION) 而远端没有，本次按重试处理：只推送，不重建"; \
	fi; \
	grep -qF 'const Version = "$(VERSION)"' version.go \
		|| { echo "❌ version.go 不是 $(VERSION)，请先更新并提交"; exit 1; }; \
	grep -qF '## [$(VERSION:v%=%)]' CHANGELOG.md \
		|| { echo "❌ CHANGELOG.md 缺少 $(VERSION) 的版本段，请先归档 [Unreleased] 并提交"; exit 1; }; \
	echo "✅ 前置检查通过：$(VERSION)"

## 发版。前置检查与完整门禁全部通过才会产生并推送 tag；任一步失败都不会打 tag。
## 用法：make tag VERSION=v1.3.0
## 需要完整的中文 tag message 时：make tag VERSION=v1.3.0 MESSAGE_FILE=/tmp/tag.txt
tag: preflight gate ## 发版：make tag VERSION=vX.Y.Z
	@set -e; \
	if git rev-parse -q --verify "refs/tags/$(VERSION)" >/dev/null; then \
		echo "ℹ️  复用已存在的本地 tag $(VERSION)"; \
	elif [ -n "$(MESSAGE_FILE)" ]; then \
		git tag -a "$(VERSION)" -F "$(MESSAGE_FILE)"; \
		echo "✅ 已创建附注标签 $(VERSION)"; \
	else \
		git tag -a "$(VERSION)" \
			-m "版本 $(VERSION)" \
			-m "主要变更见 CHANGELOG.md 中 $(VERSION) 段。"; \
		echo "✅ 已创建附注标签 $(VERSION)"; \
	fi; \
	git push --atomic gtkit main "refs/tags/$(VERSION)"; \
	echo "✅ 已原子推送 main 与 $(VERSION)"
