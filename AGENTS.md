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
make test-integration        integration tests; Linux box only, needs root
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

services/sandbox/          the orchestrator, owns the state machine
services/image/            pull, unpack, cache policy
services/bundle/           build the OCI bundle from an image config
services/state/            the sandbox record repository
services/egress/           compile and apply policy
services/secret/           grants and destination binding
services/provider/gvisor/       implements models.Provider on gVisor
services/provider/firecracker/  implements models.Provider on Firecracker
services/provider/conformance/  the test suite both substrates must pass

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

## Tests

Unit tests run anywhere and need no box. `make test` must stay green on macOS.
Anything touching `runsc`, netns or KVM goes behind `//go:build integration` and
runs through `make test-integration`. CI runs `make check` only.

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
