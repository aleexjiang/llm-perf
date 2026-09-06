BINARY := bin/bench
LINUX_BINARY := bin/bench-linux-amd64
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/aleexjiang/llm-perf/internal/report.Version=llm-perf/$(VERSION)

.PHONY: build build-linux test clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/bench

build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(LINUX_BINARY) ./cmd/bench

test:
	go test ./...

clean:
	rm -rf bin
