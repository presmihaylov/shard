BIN      := bin/shard
SHARD_INIT_BIN := bin/shard-init
PKG      := github.com/presmihaylov/shard
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -X main.version=$(VERSION)

GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.1.4

DEVBOX ?= devbox-shard

# Which packages `make itest` runs on the box. Narrow it while you work on one ticket.
ITEST_PKG ?= ./services/network/... ./services/provider/gvisor/...

.PHONY: all build build-linux build-shard-init build-shard-init-linux test test-integration e2e-test vet lint lint-fix fmt fmt-check vuln check clean devbox-sync devbox-test itest e2e devbox-e2e devbox-demo

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
# -p 1 because these tests own host state: the bridge, the nft table and /var/run/netns.
test-integration:
	go test -tags integration -p 1 ./...

# The hardened sshd runs no sftp subsystem, so scp needs -O.
devbox-sync: build-linux build-shard-init-linux
	scp -O -q $(BIN)-linux-amd64 $(SHARD_INIT_BIN)-linux-amd64 $(DEVBOX):/tmp/
	ssh $(DEVBOX) 'sudo install -m0755 /tmp/shard-linux-amd64 /usr/local/bin/shard && \
		sudo install -m0755 /tmp/shard-init-linux-amd64 /usr/local/bin/shard-init'
	@installed=$$(ssh $(DEVBOX) shard version 2>/dev/null | sed -n 's/^client //p'); \
		if [ "$$installed" != "$(VERSION)" ]; then \
			echo "the devbox runs $$installed, not $(VERSION): the install did not land"; \
			exit 1; \
		fi; \
		echo "the devbox runs $$installed"

# Runs integration tests on the box, as root, over the binaries devbox-sync just installed.
# .claude holds a worktree per in-flight ticket, which is a second checkout and never the one under test.
itest: devbox-sync
	tar czf - --exclude bin --exclude .git --exclude .claude . | ssh $(DEVBOX) 'rm -rf ~/shard && mkdir -p ~/shard && tar xzf - -C ~/shard && cd ~/shard && sudo $$(command -v go || echo /usr/local/go/bin/go) test -tags integration -count=1 -p 1 -v $(ITEST_PKG)'

# The whole suite, which is the same target with no package filter. gVisor only: Hetzner has no KVM.
devbox-test: ITEST_PKG = ./...
devbox-test: itest

# SHARD-17: the whole lifecycle on this host, the way a stranger with a fresh box would drive it, over a daemon it starts.
e2e:
	./scripts/e2e.sh

# The guards in that script decide what gets deleted, so they are tested off the box, without root.
e2e-test:
	./scripts/e2e_test.sh

# The same script on the box, over a fresh copy of this tree.
devbox-e2e:
	tar czf - --exclude bin --exclude .git --exclude .claude . | ssh $(DEVBOX) 'sudo rm -rf ~/shard-e2e && mkdir -p ~/shard-e2e && tar xzf - -C ~/shard-e2e && cd ~/shard-e2e && sudo PATH=$$PATH:/usr/local/go/bin ./scripts/e2e.sh'

# Records the SHARD-36 demo on the box, over the binaries devbox-sync just installed, into docs/demo.cast.
devbox-demo: devbox-sync
	tar czf - --exclude bin --exclude .git --exclude .claude . | ssh $(DEVBOX) 'sudo rm -rf ~/shard-demo && mkdir -p ~/shard-demo && tar xzf - -C ~/shard-demo && cd ~/shard-demo && sudo asciinema rec --overwrite --cols 120 --rows 40 -c ./scripts/demo.sh demo.cast'
	ssh $(DEVBOX) 'cat ~/shard-demo/demo.cast' > docs/demo.cast

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

check: fmt-check vet lint test e2e-test

clean:
	rm -rf bin
