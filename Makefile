BINARY := bin/bench
LINUX_BINARY := bin/bench-linux-amd64

.PHONY: build build-linux test clean

build:
	go build -o $(BINARY) ./cmd/bench

build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o $(LINUX_BINARY) ./cmd/bench

test:
	go test ./...

clean:
	rm -rf bin
