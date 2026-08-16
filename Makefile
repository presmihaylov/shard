BIN      := bin/shard
SHARD_INIT_BIN := bin/shard-init
PKG      := github.com/presmihaylov/shard
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -X main.version=$(VERSION)

GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.1.4

DEVBOX ?= devbox-shard

.PHONY: all build build-linux build-shard-init build-shard-init-linux test test-integration vet lint lint-fix fmt fmt-check vuln check clean devbox-sync devbox-test

all: check build

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/shard

# shard is a Linux-only server tool; the dev Mac cross-compiles and scps the binary.
build-linux:
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BIN)-linux-amd64 ./cmd/shard

# The supervisor is PID 1 in the guest, so it is static: the image may be musl or have no libc.
build-shard-init:
	CGO_ENABLED=0 go build -o $(SHARD_INIT_BIN) ./cmd/shard-init

# The guest arch must match the box that runs the sandbox, so this one ships beside shard.
build-shard-init-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o $(SHARD_INIT_BIN)-linux-amd64 ./cmd/shard-init

test:
	go test ./...

# Needs runsc, netns or KVM, so it only runs on the Linux box, as root.
test-integration:
	go test -tags integration ./...

# The hardened sshd runs no sftp subsystem, so scp needs -O.
devbox-sync: build-linux build-shard-init-linux
	scp -O -q $(BIN)-linux-amd64 $(SHARD_INIT_BIN)-linux-amd64 $(DEVBOX):/tmp/
	ssh $(DEVBOX) 'sudo install -m0755 /tmp/shard-linux-amd64 /usr/local/bin/shard && \
		sudo install -m0755 /tmp/shard-init-linux-amd64 /usr/local/bin/shard-init && shard version'

# Runs the integration suite on the box, as root. gVisor only: Hetzner has no KVM.
devbox-test: devbox-sync
	tar czf - --exclude bin --exclude .git . | ssh $(DEVBOX) 'rm -rf ~/shard && mkdir -p ~/shard && tar xzf - -C ~/shard && cd ~/shard && sudo $$(command -v go || echo /usr/local/go/bin/go) test -tags integration ./...'

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
