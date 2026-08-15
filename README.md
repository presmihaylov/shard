# shard
This is a work in progress, will announce when it's live and ready to be used!

A single-node sandbox manager. One binary runs isolated sandboxes on a Linux host, with or without
hardware virtualization, and gives them the same lifecycle verbs either way: run, exec, sleep, wake
and fork. On a host with `/dev/kvm` it drives Firecracker microVMs; on a host without one it drives
gVisor. An optional `shard serve` exposes the same verbs over a self-hostable REST API.

**Status: pre-alpha.** Nothing here works yet. The repository is a skeleton.

## Development

`make check` runs the same gates as CI: format check, vet, lint and tests.
`make fmt` and `make lint-fix` apply what can be fixed automatically.
Linting needs [golangci-lint](https://golangci-lint.run/) v2 (`brew install golangci-lint`).

The code sits in three buckets: `models/` for domain structs, `pkg/` for thin drivers over external
things, and `services/` for business logic. `pkg/` never imports `models/`, and `depguard` enforces
that in CI. [AGENTS.md](AGENTS.md) has the full layout and the rules that go with it.

`shard` is a Linux server tool and does not run on macOS. `make test` stays green on a Mac; anything
that needs `runsc`, netns or KVM lives behind the `integration` build tag and runs on a Linux box
through `make test-integration`.

`CLAUDE.md` is a symlink to `AGENTS.md`, so one document serves every agent. A Windows checkout
needs `core.symlinks=true`.

## API stability

The module stays at `v0` until launch and promises nothing. The provider interface is scheduled to
change twice, once after each substrate is real.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
