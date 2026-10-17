SHELL := /bin/bash
# 用法：
#   make hub          # 当前平台交叉编译 hub（仅编译，输出 bin/）
#   make agent        # 当前平台交叉编译 agent
#   make all / make   # hub + agent，当前平台
#   make dist         # hub + agent × linux/amd64 + linux/arm64（生产交付矩阵）
#   make openapi OUT=docs   # 重新生成 docs/openapi.json 与 docs/openapi.yaml
#   make verify-dist       # dist 后产物校验：架构/大小/静态性/版本注入（tools/verify_dist.py）
#   make test / vet / clean
#
#   GO=... 覆盖 go 二进制路径（沙箱里 go 在 /usr/local/go/bin/go）。
#   构建矩阵仅列 linux/amd64、linux/arm64；要扩架构在 PLATFORMS 里加。
GO      ?= /usr/local/go/bin/go
PKG     := github.com/yy1319hua/dockdeck

# 构建元数据：标签优先，无标签回退到短 commit，未提交改动标 -dirty。
# 同时经 -ldflags 注入 internal/version，供运行时回显。
VERSION   := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE      := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# netgo / osusergo 强制纯 Go 网络与用户解析（不依赖 libc 的 getaddrinfo/getpwuid），配合 CGO_ENABLED=0 得纯静态单二进制。
TAGS      := netgo,osusergo
EXTLD     := -extldflags "-static"
LDFLAGS   := -s -w $(EXTLD) \
             -X $(PKG)/internal/version.Version=$(VERSION) \
             -X $(PKG)/internal/version.Commit=$(COMMIT) \
             -X $(PKG)/internal/version.Date=$(DATE)
BIN       := bin
CMDS      := hub agent
PLATFORMS := linux/amd64 linux/arm64

.PHONY: all hub agent dist openapi verify-dist test vet clean info $(CMDS)

all: $(CMDS)

# --- 当前平台单 binary ---
# 仅 Linux：-dynlink 之类的运行时态在交叉静态里会出问题，此处一律静态。
hub agent:
	@mkdir -p $(BIN)
	CGO_ENABLED=0 $(GO) build -trimpath -tags '$(TAGS)' -ldflags '$(LDFLAGS)' \
	  -o $(BIN)/dockdeck-$@ ./cmd/$@
	@echo "built: $@  version=$(VERSION) commit=$(COMMIT)"

# --- 交付矩阵：hub/agent × PLATFORMS，产物 bin/dockdeck-<cmd>-<os>-<arch> ---
dist:
	@for cmd in $(CMDS); do \
	  for p in $(PLATFORMS); do \
	    os=$${p%/*}; arch=$${p#*/}; \
	    CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -tags '$(TAGS)' -ldflags '$(LDFLAGS)' \
	      -o $(BIN)/dockdeck-$$cmd-$$os-$$arch ./cmd/$$cmd || exit 1; \
	    echo "built: dockdeck-$$cmd-$$os-$$arch"; \
	  done; \
	done

# --- 产物校验（dist 之后跑）：架构 / 大小 / 静态性 / 版本注入 ---
# tools/verify_dist.py：stdlib-only 机读 ELF 头（不依赖 file/binutils）。
# 检查项与预算见脚本头注；--commit 默认取本文件的 COMMIT（与 dist 同源 ldflags）。
verify-dist:
	python3 tools/verify_dist.py --bin $(BIN) --commit $(COMMIT)

# --- 重生成 OpenAPI 产物（同源，与实装同一份路由登记表） ---
# 必须用「无制品版本的默认 binary」生成：仓库内 docs/openapi.json(yaml) 的 info.version
# 约定为占位 "dev"（internal/version 默认值），由部署侧运行时注入真值。这里若带制品版
# 本的 ldflags 生成，info.version 会是 commit 段，与 TestCommittedSpecMatchesGenerated
# 的默认基准不一致而红，故显式把 VERSION/COMMIT/DATE 覆盖为占位。
openapi:
	@if [ -z "$(OUT)" ]; then echo "usage: make openapi OUT=<dir>"; exit 2; fi
	CGO_ENABLED=0 $(GO) build -trimpath -tags '$(TAGS)' -ldflags '-X $(PKG)/internal/version.Version=dev -X $(PKG)/internal/version.Commit=dev -X $(PKG)/internal/version.Date=dev' -o $(BIN)/openapi-gen ./cmd/hub
	$(BIN)/openapi-gen --dump-openapi "$(OUT)"
	@rm -f $(BIN)/openapi-gen
	@echo "wrote $(OUT)/openapi.json  and  $(OUT)/openapi.yaml"

test:
	TZ=Asia/Shanghai CGO_ENABLED=0 $(GO) test ./...

vet:
	CGO_ENABLED=0 $(GO) vet ./...

clean:
	rm -rf $(BIN)

info:
	@echo "PKG     = $(PKG)"
	@echo "VERSION = $(VERSION)"
	@echo "COMMIT  = $(COMMIT)"
	@echo "DATE    = $(DATE)"

