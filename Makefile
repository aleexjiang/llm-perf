# 构建产物统一落 bin/，命名 bench-<os>-<arch>（禁止无架构后缀的裸名，避免混淆平台）。
# 版本号经 -ldflags 注入 report.Version（git describe），随 JSON 落盘可追溯。

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/aleexjiang/llm-perf/internal/report.Version=llm-perf/$(VERSION)

# 交叉构建参数（build-os / build-linux 消费）：默认 linux/amd64（客户堡垒机主目标）。
GOOS   ?= linux
GOARCH ?= amd64

.PHONY: build build-linux build-os test clean

# 本机构建：产物 = bin/bench-<本机os>-<本机arch>
build:
	$(eval BOS := $(shell go env GOOS))
	$(eval BARCH := $(shell go env GOARCH))
	$(MAKE) build-os GOOS=$(BOS) GOARCH=$(BARCH)

# 客户交付主目标（linux/amd64，堡垒机 x86 CentOS）
build-linux:
	$(MAKE) build-os GOOS=linux GOARCH=amd64

# 泛化交叉构建：make build-os GOOS=linux GOARCH=arm64
# 静态编译（CGO_ENABLED=0），产出 bin/bench-$(GOOS)-$(GOARCH)
build-os:
	@test -n "$(GOOS)" && test -n "$(GOARCH)" || (echo "GOOS/GOARCH 不能为空" && exit 1)
	@mkdir -p bin
	GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/bench-$(GOOS)-$(GOARCH) ./cmd/bench
	@echo "built bin/bench-$(GOOS)-$(GOARCH) (version llm-perf/$(VERSION))"

test:
	go test ./...

clean:
	rm -rf bin
