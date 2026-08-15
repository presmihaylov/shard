# AGENTS.md

Instructions for any agent or human working in this repository.

`CLAUDE.md` is a symlink to this file. One document serves every agent. Edit this
file, never the symlink. A Windows checkout needs `core.symlinks=true`.

`shard` is a single-node sandbox manager. One binary runs isolated sandboxes on a
Linux host with or without hardware virtualization, and gives them the same
lifecycle verbs either way. On a host with `/dev/kvm` it drives Firecracker; on a
host without one it drives gVisor.

**Status: pre-alpha.** The plan of record is `~/prg/specs/wip/nairi-sandbox/tasks.md`,
outside this repository. Ticket IDs below (`SHARD-nn`) refer to it.

---

## Commands

```
make build       build ./cmd/shard into bin/shard
make build-linux cross-compile for the box (GOOS=linux GOARCH=amd64)
make test        unit tests; must stay green on macOS
make test-integration   integration tests; Linux box only, needs root
make vet         go vet
make lint        golangci-lint (v2: brew install golangci-lint)
make lint-fix    apply the fixes golangci-lint can make
make fmt         apply formatting
make check       fmt-check + vet + lint + test, the same gates as CI
make vuln        govulncheck
```

`make check` must pass before every commit.

---

## Layout

Three buckets, plus `cmd/` and `cli/` which belong to none of them.

```
cmd/shard/                 main only, thin: wire dependencies and exit
cli/                       command definitions and flag parsing

models/                    domain structs, no behavior, no I/O
pkg/                       thin drivers over external things, no domain policy
  runsc/                   the runsc binary
  firecracker/             the firecracker binary and its API socket   (chunk 5)
  registry/                OCI registry transport
  netns/                   netns, veth, bridge, NAT rules
  store/                   atomic file write, lockfile
  proxy/                   intercepting HTTP and TLS proxy             (chunk 4)

services/                  business logic, owns the rules
  sandbox/                 the orchestrator, owns the state machine
  image/                   pull, unpack, cache policy
  bundle/                  build the OCI bundle from an image config
  provider/gvisor/         implements models.Provider on gVisor
  provider/firecracker/    implements models.Provider on Firecracker   (chunk 5)
  state/                   the sandbox record repository
  egress/                  compile and apply policy                    (chunk 4)
  secret/                  grants and destination binding

docs/
```

### The four layout rules

1. **`pkg/` must never import `models/`.** That single line is the whole boundary.
   If a `pkg` package needs a domain type, it is not a driver any more and it
   belongs in `services/`. `depguard` enforces this in CI, so drift fails the
   first pull request instead of surfacing in month three.
2. **`models/` is one package with several files, and it is a leaf.** It imports
   nothing else in this module. Do not split it per concern: the concerns
   reference each other, and split packages that do so form an import cycle,
   which Go refuses to compile.
3. **The `Provider` interface lives in `models/`.** Go idiom says define an
   interface where you consume it. Both provider implementations and `cli` need
   it, so a shared home wins here. This is deliberate. Do not move it back.
4. **`services/provider/` holds no Go code of its own.** It is a parent
   directory, so the two implementations stay visibly siblings. The whole pitch
   is that they are equals.

`pkg/firecracker` and `services/provider/firecracker` share a package name. That
is legal, but a file importing both must alias one. Alias the driver, as `fcapi`.

### API stability

The module stays at `v0` until launch. Semver and Go modules both say a `v0.x`
module promises nothing, and that is what covers the two scheduled interface
revisions (SHARD-15 after gVisor, SHARD-45 after Firecracker). Expect the
`Provider` interface to change twice. The failure mode to avoid is not "designed
early", it is "refused to change later".

---

## Platform

**Nothing that calls `runsc`, netns or KVM runs on macOS.** The dev Mac is arm64
and has no `runsc` port. `shard` is a server-side Linux tool and always was.

- Build for the box with `make build-linux`, then `scp` the binary.
- The box of record is one x86_64 VPS with no `/dev/kvm` (SHARD-9). x86_64
  matters, because every published benchmark must match the lab measurements.
- Any package a developer might import must still compile on macOS. Keep types
  and interfaces pure Go, and put anything that shells out to `runsc` behind a
  Linux build tag with a stub that returns a clear unsupported-platform error.

## Tests

- Unit tests run anywhere and need no box. `make test` must stay green on macOS.
- Anything that touches `runsc`, netns or KVM goes behind `//go:build integration`
  and runs only on a box, through `make test-integration`.
- CI runs `make check` only. Integration tests are not in CI yet.

---

## Rules that outrank convenience

**Refuse, never downgrade.** An unsupported verb fails fast with a readable error
that names the provider and the verb. Never silently fall back to a weaker
mechanism. `gondolin` degrades KVM to TCG transparently and the result is a
sandbox that is quietly too slow to use. Capability flags are per provider, one
boolean per optional verb. Never pretend providers are equal.

**Never log a secret value, and never write one into a state file.** A sandbox
references a secret by name and never holds a value. A secret is granted to a
destination, never to a sandbox alone.

**Host netfilter is the policy of record.** gVisor netstack iptables do NOT
survive checkpoint and restore. Treat netstack rules as defence in depth, and
re-apply them after every restore.

**Drive bare `runsc`, with no Docker and no containerd.** gVisor checkpoint and
restore are unreachable through the containerd shim (containerd#12280), and
`runsc restore` fails under Docker on a shim state conflict.

---

## Code style

- Avoid `else`. An early return reads better than an indented branch.
- Comments explain the non-obvious why. Skip what the code already says.
- Wrap errors with context (`fmt.Errorf("...: %w", err)`). Never discard one.
- One word for one thing. The noun is `sandbox`, in the CLI, the docs, the state
  records and the API. Never `instance`, never `box`, never `container`.

---

## Git

- Branch per ticket: `shard-<n>-<slug>`. Merge through a pull request.
- **Push over SSH, not HTTPS.** An HTTPS push authenticates as the GitHub CLI
  OAuth app, whose token has no `workflow` scope, so any push touching
  `.github/workflows/` is rejected on every branch.
