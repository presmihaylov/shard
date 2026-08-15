BIN     := bin/shard
PKG     := github.com/presmihaylov/shard
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.1.4

.PHONY: all build build-linux test vet lint lint-fix fmt fmt-check vuln check clean

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

# Needs golangci-lint v2: brew install golangci-lint
lint:
	golangci-lint run ./...

lint-fix:
	golangci-lint run --fix ./...

fmt:
	golangci-lint fmt

fmt-check:
	golangci-lint fmt --diff

vuln:
	go run $(GOVULNCHECK) ./...

check: fmt-check vet lint test

clean:
	rm -rf bin
