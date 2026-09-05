# AGENTS.md

`shard` is a single-node sandbox manager. One binary runs isolated sandboxes on a
Linux host with or without hardware virtualization, and gives them the same
lifecycle verbs either way. With `/dev/kvm` it drives Firecracker; without one it
drives gVisor.

`CLAUDE.md` is a symlink to this file. Edit this file, never the symlink. A
Windows checkout needs `core.symlinks=true`.

Status: pre-alpha. `SHARD-nn` refers to the task list, which lives outside this
repository.

## Commands

```
make build                   build ./cmd/shard into bin/shard
make build-linux             cross-compile for the box (GOOS=linux GOARCH=amd64)
make build-shard-init        build ./cmd/shard-init into bin/shard-init (static, CGO_ENABLED=0)
make build-shard-init-linux  cross-compile the supervisor for the box
make test                    unit tests; must stay green on macOS
make test-integration        integration tests, on this host; Linux box only, needs root
make itest                   integration tests for ITEST_PKG, on the devbox
make e2e                     the whole lifecycle on this host, as root, over a daemon it starts (SHARD-17)
make devbox-e2e              the same script, on the devbox
make devbox-demo             record scripts/demo.sh on the devbox into docs/demo.cast (SHARD-36)
make lint                    golangci-lint (v2: brew install golangci-lint)
make lint-fix                apply the fixes golangci-lint can make
make fmt                     apply formatting
make check                   the same gates as CI; must pass before every commit
make vuln                    govulncheck
```

## Layout

Code goes in one of three buckets. `cmd/` and `cli/` belong to none of them.

- **`models/`** domain structs. No behavior, no I/O.
- **`pkg/`** thin drivers over external things. Mechanism, no domain policy.
- **`services/`** business logic. Owns the rules.

Create a package when the ticket that needs it lands, not before. The tree below
is the shape to grow into, not a checklist to build up front.

```
cmd/shard/                 main only, thin: wire dependencies and exit
cmd/shard-init/            the guest supervisor, PID 1 in every sandbox
cli/                       command definitions and flag parsing

models/                    Sandbox, states, Provider, Capabilities, Policy

pkg/runsc/                 the runsc binary
pkg/firecracker/           the firecracker binary and its API socket
pkg/registry/              OCI registry transport
pkg/netns/                 netns, veth, bridge, NAT rules
pkg/store/                 atomic file write, lockfile
pkg/proxy/                 intercepting HTTP and TLS proxy

services/sandbox/          the orchestrator: the lifecycle verbs the daemon serves
services/image/            pull, unpack, cache policy
services/bundle/           build the OCI bundle from an image config
services/sandboxstate/     the sandbox record repository
services/egress/           compile and apply policy
services/secret/           grants and destination binding
services/daemon/           shard daemon, the supervision framework for the background work
services/api/              the REST handlers the daemon serves over its unix socket
services/client/           the typed client of that API, which the thin CLI verbs call
services/provider/gvisor/       implements models.Provider on gVisor
services/provider/firecracker/  implements models.Provider on Firecracker
services/provider/conformance/  the test suite both substrates must pass

packaging/systemd/         the unit that installs shard daemon as a resident process
docs/
```

### Rules

- **`pkg/` never imports `models/`.** If a driver needs a domain type, it is not
  a driver and it belongs in `services/`. `depguard` enforces this in CI.
- **Dependencies point one way: `cli` to `services` to `pkg`.** `models` sits
  under all of them.
- **`models/` is one package with several files, and it is a leaf.** It imports
  nothing else in the module. Splitting it per concern creates import cycles.
- **The `Provider` interface lives in `models/`.** Both provider implementations
  and `cli` need it, so it does not live at a single consumer. Do not move it.
- **`services/provider/` holds no Go code of its own.** It is a parent directory
  only, so the substrates stay siblings. `conformance/` is the one sibling that
  is not a substrate: it is the suite they both import from their own tests.
- **Name a `pkg` after the thing it drives, and a provider after the substrate.**
  So `pkg/runsc` drives the binary, `services/provider/gvisor` is the substrate.
  `pkg/firecracker` and `services/provider/firecracker` therefore collide: a file
  importing both must alias the driver, as `fcapi`.
- The module stays at `v0`. Expect the `Provider` interface to change as each
  substrate lands.

## Platform

**Nothing that calls `runsc`, netns or KVM runs on macOS.** `shard` is a Linux
server tool. Build with `make build-linux` and `scp` to an x86_64 box.

Any package a developer might import must still compile on macOS. Keep types and
interfaces pure Go, and put anything that shells out behind a Linux build tag,
with a stub that returns a clear unsupported-platform error.

### The devbox

`ssh devbox-shard` is a throwaway Debian 13 box with `runsc` and Go on it. It is
the `shard` fleet in `nairi-infra`, provisioned by `make provision TARGET=shard`.

```
make devbox-sync     build for linux and install the two binaries on the box
make itest           the same, then run ONE package's integration tests there as root
make devbox-test     the same, for every package
```

`itest` is the loop to use while a ticket is in flight. It installs the two binaries,
copies the source to `~/shard` on the box, and runs
`go test -tags integration -count=1 -v $(ITEST_PKG)` as root. `ITEST_PKG` defaults to
the package the current ticket owns; override it per run:

```
make itest ITEST_PKG=./services/image/...
```

`devbox-test` is the same target with `ITEST_PKG=./...`.

Hetzner Cloud exposes no `/dev/kvm`, so the devbox covers gVisor only.
Firecracker needs a dedicated server, which is a decision for SHARD-20.

## Tests

Unit tests run anywhere and need no box. `make test` must stay green on macOS.
Anything touching `runsc`, netns or KVM goes behind `//go:build integration` and
runs through `make itest`. CI runs `make check` only.

**The build tag is the whole mechanism.** A file whose first line is `//go:build
integration` does not exist to the compiler without `-tags integration`, so plain
`go test ./...` never sees it and can never skip it. The `_integration_test.go`
suffix is for humans; keep both. Every such test also guards itself at runtime
(`euid == 0`, the binary is on PATH) and skips rather than fails.

## Rules that outrank convenience

- **Refuse, never downgrade.** An unsupported verb fails fast with an error that
  names the provider and the verb. Never fall back to a weaker mechanism.
  Capabilities are per provider, one boolean per optional verb.
- **A sandbox outlives its entrypoint.** When the entrypoint exits the sandbox
  stays `running`, and you can still exec, pause or fork it. `stop` is the only
  thing that ends one. There is no policy, no idle timer and no on-exit setting
  to change any of this. This is why `shard-init` is PID 1 in every sandbox and
  the image entrypoint is its child.
- **Never log a secret value, and never write one into a state file.** A sandbox
  references a secret by name and never holds a value. A secret is granted to a
  destination, never to a sandbox alone.
- **Host netfilter is the policy of record.** gVisor netstack iptables do not
  survive checkpoint and restore, so re-apply them after every restore.
- **Drive bare `runsc`. No Docker, no containerd.** gVisor checkpoint and restore
  are unreachable through the containerd shim (containerd#12280).

## Code style

- Avoid `else`. An early return reads better than an indented branch.
- **One line per comment, no exceptions.** A comment explains the non-obvious why
  and never what the code already says. If the reason needs a paragraph, it
  belongs in `docs/`, in the ticket, or in the commit message, not above the
  declaration.
- **Run a deslop round before every commit.** Re-read the diff and cut what a
  human would not have written: restated code, multi-line comment blocks,
  defensive checks nothing calls, and anything that does not match the file
  around it.
- **Handle every error explicitly.** Return it, wrapped with context
  (`fmt.Errorf("...: %w", err)`). Never swallow one, never log and continue, and
  never assign one to `_`. If an error is genuinely not worth propagating, that
  is a decision to be asked for, not assumed: the only exception is an explicit
  instruction to "silently log and continue", granted per case, and recorded in
  a comment at the call site that says who decided it and why.
- One word for one thing. The noun is `sandbox`, in the CLI, the docs, the state
  records and the API. Never `instance`, never `box`, never `container`.

## Git

- Branch per ticket: `shard-<n>-<slug>`. Merge through a pull request.
- **Push over SSH, not HTTPS.** An HTTPS push authenticates as the GitHub CLI
  OAuth app, whose token has no `workflow` scope, so any push touching
  `.github/workflows/` is rejected.
