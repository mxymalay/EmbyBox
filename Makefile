# EmbyBox Makefile
#
# 常用命令。查看全部：make help

GO       ?= go
BIN      ?= embybox
BIN_DIR  ?= .
CMD      ?= ./cmd/embybox

# 版本信息嵌入二进制
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS  := -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.DEFAULT_GOAL := help

# ─────────────────────────────────────────────
# 开发
# ─────────────────────────────────────────────

.PHONY: run
run: ## 本地启动
	$(GO) run $(CMD)

.PHONY: build
build: ## 编译二进制
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BIN) $(CMD)

.PHONY: build-all
build-all: ## 交叉编译全平台
	@mkdir -p dist
	GOOS=linux   GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o dist/$(BIN)-linux-amd64   $(CMD)
	GOOS=linux   GOARCH=arm64 $(GO) build -ldflags "$(LDFLAGS)" -o dist/$(BIN)-linux-arm64   $(CMD)
	GOOS=darwin  GOARCH=arm64 $(GO) build -ldflags "$(LDFLAGS)" -o dist/$(BIN)-darwin-arm64  $(CMD)
	GOOS=windows GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o dist/$(BIN)-windows-amd64.exe $(CMD)
	@echo "产物在 dist/"

# ─────────────────────────────────────────────
# 质量
# ─────────────────────────────────────────────

.PHONY: test
test: ## 跑全部测试
	$(GO) test ./...

.PHONY: test-race
test-race: ## 跑测试并开启竞态检测（CI 用）
	$(GO) test ./... -race

.PHONY: cover
cover: ## 生成覆盖率报告
	$(GO) test ./... -coverprofile=coverage.out
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "报告：coverage.html"

.PHONY: fmt
fmt: ## 格式化
	$(GO) fmt ./...

.PHONY: vet
vet: ## 静态检查
	$(GO) vet ./...

.PHONY: check
check: fmt vet test-race build ## 提交前完整自查

# ─────────────────────────────────────────────
# 项目特有的检查
# ─────────────────────────────────────────────

.PHONY: purity
purity: ## 检查内核里没有业务名词
	@./scripts/check-kernel-purity.sh

.PHONY: migrations
migrations: ## 验证迁移在空库与有数据的库上都能跑
	@./scripts/test-migrations.sh

.PHONY: docs
docs: ## 检查文档内部链接
	@./scripts/check-docs.sh

# ─────────────────────────────────────────────
# 运维辅助
# ─────────────────────────────────────────────

.PHONY: refs
refs: ## 拉取参考项目（约 170 MB）
	@./scripts/fetch-references.sh

.PHONY: clean
clean: ## 清理构建产物
	@rm -rf dist coverage.out coverage.html
	@echo "已清理"

.PHONY: help
help: ## 显示本帮助
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'
