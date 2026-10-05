# shard

A single-node sandbox manager for Linux and macOS.

Create sandboxes from OCI images, execute commands, pause and resume processes, and fork live
sandboxes. One binary provides the CLI and the daemon, with gVisor, Sysbox, runc, Firecracker,
and Virtualization.framework providers.

## Why shard?

Give an agent a sandbox that outlives its commands. Keep its files between commands, save its
memory when idle, or fork its current state to try another path. Use the same lifecycle commands
on a Linux server and an Apple silicon Mac, with secrets and outbound network policy under host
control. shard manages one host; it does not schedule a fleet.

## 60-second quickstart

This example uses gVisor on Linux. [Install shard](#install), `shard-init`, `runsc`, `iproute2`,
and `nftables` first. Linux providers need root. The examples use their own data directory,
`/var/lib/shard-demo`, selected by `--root`.

Start the daemon in one terminal and leave it there:

```sh
sudo shard --root /var/lib/shard-demo daemon --provider gvisor
```

In a second terminal, create a sandbox, write a file, read it, and remove the sandbox:

```sh
sudo shard --root /var/lib/shard-demo create --name demo --memory 512MiB alpine:3.20
sudo shard --root /var/lib/shard-demo exec demo sh -c 'echo hello from shard > /tmp/hello.txt'
sudo shard --root /var/lib/shard-demo exec demo cat /tmp/hello.txt
sudo shard --root /var/lib/shard-demo remove --force demo
```

The file command prints `hello from shard`. `create` starts no main command, so the sandbox
stays available for `exec`. The daemon downloads an image on its first use.

For a Mac, follow [the Mac setup](docs/mac.md). The supported host is Apple silicon with macOS
14 or later; its `vz` daemon runs as your user and needs neither Docker nor root.

## Install

### Release binaries

Use these assets from the [GitHub releases](https://github.com/presmihaylov/shard/releases)
when v0.1.0 is available. Until then, [build from source](#build-from-source).

| Host | Assets |
|---|---|
| Linux x86-64 | `shard-linux-amd64` and `shard-init-linux-amd64` |
| macOS Apple silicon | `shard-darwin-arm64` |
| macOS Intel, client use only | `shard-darwin-amd64` |
| Checksums | `SHA256SUMS` |

On Linux, download both binaries and install them as `shard` and `shard-init`:

```sh
sudo install -m0755 shard-linux-amd64 /usr/local/bin/shard
sudo install -m0755 shard-init-linux-amd64 /usr/local/bin/shard-init
```

Install the runtime for your provider separately. gVisor needs `runsc`; Sysbox needs
`sysbox-runc`; runc needs `runc`; Firecracker needs `firecracker`, `jailer`, and `/dev/kvm`.
The [provider contract](docs/provider.md) lists the host requirements and limits.

The Mac binary embeds its VM shim and guest supervisor. An Intel Mac can use the CLI with a
remote Linux daemon; a local Intel Mac daemon is unsupported.

### Build from source

Use the Go version in [go.mod](go.mod), Git, and Make:

```sh
git clone https://github.com/presmihaylov/shard.git
cd shard
make build-linux build-shard-init-linux
sudo install -m0755 bin/shard-linux-amd64 /usr/local/bin/shard
sudo install -m0755 bin/shard-init-linux-amd64 /usr/local/bin/shard-init
```

On a Mac, install the Xcode Command Line Tools and use `make build-darwin` instead. It produces
`bin/shard-darwin-arm64` on Apple silicon. See [the release guide](docs/release.md) for the build
and verification process.

## Providers

| Provider | Host | Isolation | Pause, resume, fork |
|---|---|---|---|
| gVisor (`gvisor`) | Linux | User-space kernel | Yes |
| Sysbox (`sysbox`) | Linux | User namespace | No |
| runc (`runc`) | Linux | Host kernel, no user namespace | No |
| Firecracker (`firecracker`) | Linux with `/dev/kvm` | MicroVM | Yes |
| Virtualization.framework (`vz`) | Apple silicon, macOS 14+ | VM | Yes |

Every provider supports create, exec, stop, start, remove, and filesystem snapshots.
Unsupported verbs fail with the provider name; shard never substitutes another mechanism.

A new root defaults to Firecracker when the daemon can open `/dev/kvm`, gVisor on Linux without
usable KVM, and `vz` on macOS. An existing root keeps its provider. Sysbox and runc require an
explicit selection. **Sysbox is single-tenant. Use runc only for code you trust.** Read the
[full provider matrix](docs/provider.md) before you select a provider.

## Core concepts

| Concept | Behavior |
|---|---|
| Sandbox | `create` starts without a main command. `run` starts the command you supply. The image's ENTRYPOINT and CMD never run. |
| Exec | Execute another command in a running sandbox. Use `-i` / `--interactive` for stdin; `-t` / `--tty` requires it. |
| Stop and start | Stop frees memory and keeps files. Start uses those files and starts any main command from the beginning. |
| Pause and resume | Save memory and files, then continue the processes from that state. Availability depends on the provider. |
| Fork | Capture a running sandbox's memory and files as a new sandbox. The source briefly pauses, then continues. |
| Snapshot | Save a stopped sandbox's files without memory. The snapshot outlives its source and can seed a new sandbox. |
| Secrets | The sandbox receives placeholders. The host proxy replaces them in HTTPS request headers sent to granted destinations. |
| Egress | Without a policy, a sandbox can reach the internet but not private networks. A policy adds ordered allow and deny rules. |

A sandbox stays running after its main command exits. Only `stop` or `remove` ends it.
For example:

```sh
sudo shard --root /var/lib/shard-demo run --name job --memory 512MiB alpine:3.20 echo job done
sudo shard --root /var/lib/shard-demo exec job echo sandbox still available
sudo shard --root /var/lib/shard-demo stop job
sudo shard --root /var/lib/shard-demo snapshot create --name job-files job
sudo shard --root /var/lib/shard-demo create --name job-copy --memory 512MiB --snapshot job-files
sudo shard --root /var/lib/shard-demo remove --force job-copy
sudo shard --root /var/lib/shard-demo remove job
sudo shard --root /var/lib/shard-demo snapshot remove job-files
```

When you finish the examples, press Ctrl+C in the daemon terminal.

Flags precede the image or sandbox name; the command follows it. `--memory` and `--disk` take
sizes such as `512MiB` or `2GiB`; `--vcpus` takes a whole number. Secret destinations use
`--destination` or its alias `--dest`. Grant secrets only to destinations that never return
the credential in a response. See [the CLI reference](docs/cli.md),
[the lifecycle](docs/state-machine.md), [secrets](docs/secrets.md), and [egress](docs/egress.md).

## API and SDKs

The daemon serves a REST API over a Unix socket. For remote access, `shard serve` checks API
tokens and forwards requests to that socket. Put an HTTPS proxy in front for public access.
The CLI and SDKs use `SHARD_REMOTE` and `SHARD_API_KEY` to connect.
See [the daemon and API guide](docs/daemon.md) and [the OpenAPI schema](docs/openapi.json).

The `useshards` SDKs live in [sdks/typescript](sdks/typescript) and [sdks/python](sdks/python).
They provide sandbox commands, files, lifecycle operations, secrets, and policies. TypeScript
requires Node.js 20.3 or later; Python requires Python 3.11 or later and has synchronous and
asynchronous clients.

Install them with `npm install useshards` or `pip install useshards`. The source and examples
are in those directories, and [the release guide](docs/release.md) says how an SDK release is made.

## Documentation

- [CLI commands and options](docs/cli.md)
- [Daemon, REST API, and remote access](docs/daemon.md)
- [Provider capabilities and limits](docs/provider.md)
- [Mac setup](docs/mac.md) and [the Mac provider](docs/provider-vz.md)
- [Lifecycle](docs/state-machine.md), [files](docs/files.md), [secrets](docs/secrets.md), and [egress](docs/egress.md)

## Status and contributions

**Pre-alpha.** The API, CLI, and SDKs can change without compatibility guarantees.

Read [AGENTS.md](AGENTS.md) for the code layout and contribution rules. Install the
`golangci-lint` version in [CI](.github/workflows/ci.yml), then run `make check` before a commit.
It checks format, vet, lint, unit tests, and the e2e script's own tests.
Tests that need a runtime, namespaces, or KVM use the `integration` build tag and run on a
suitable host. See [the release guide](docs/release.md) for the platform checks.

## License

[Apache-2.0](LICENSE). See [NOTICE](NOTICE) for attribution.
