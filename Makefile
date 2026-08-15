BIN     := bin/shard
PKG     := github.com/presmihaylov/shard
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: all build build-linux test vet fmt check clean

all: check build

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/shard

# shard is a Linux-only server tool; the dev Mac cross-compiles and scps the binary.
build-linux:
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BIN)-linux-amd64 ./cmd/shard

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check: vet test

clean:
	rm -rf bin
