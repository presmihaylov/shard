# shard

A single-node sandbox manager for Linux and macOS.

shard creates sandboxes from OCI images, runs commands in them, pauses and resumes their processes,
and forks live sandboxes. One binary is both the CLI and the daemon, and it drives five providers:
gVisor, Sysbox, runc, Firecracker, and Virtualization.framework.

## Why shard?

shard gives an agent a sandbox that outlives its commands. The agent's files stay between commands.
You can save its memory while it is idle, or fork its current state to try another path. The same
lifecycle commands work on a Linux server and on an Apple silicon Mac, and the host keeps control of
secrets and outbound network policy. shard manages one host. It does not schedule a fleet.

## 60-second quickstart

Install the CLI, then set up this machine as a sandbox host:

```sh
curl -fsSL https://useshards.com/install | sh
shard setup
```

The installer puts `shard` in `~/.local/bin`. If that directory is not on your `PATH`, it prints the
line that adds it. When a terminal is attached, the installer also offers to start `shard setup`.
Setup asks which provider to use and whether the daemon starts at boot, shows what it will change,
and asks once before it changes anything. Then it installs the provider's tools. If you answer Yes
at boot, the recommended choice, setup also starts the daemon as a systemd service on Linux or a
launchd service on a Mac. If you answer No, it prints the `shard daemon` command to run yourself.

Create a sandbox, write a file, read it, and remove the sandbox:

```sh
sudo shard create --name demo --memory 512MiB alpine:3.20
sudo shard exec demo sh -c 'echo hello from shard > /tmp/hello.txt'
sudo shard exec demo cat /tmp/hello.txt
sudo shard remove --force demo
```

The third command prints `hello from shard`. `create` starts no main command, so the sandbox stays
available for `exec`. The daemon downloads an image the first time a sandbox uses it. On Linux the
API socket belongs to root, so local commands run with `sudo`. On a Mac the daemon runs as your
user, so drop `sudo`.

To host sandboxes, a machine needs Linux on x86-64, or Apple silicon with macOS 14 or later. Any
other machine, such as an Intel Mac, can still use the CLI, and `shard setup` connects it to a
remote server. See [the setup guide](docs/setup.md).

## Install

```sh
curl -fsSL https://useshards.com/install | sh
```

The installer downloads the newest release for this platform, checks it against the release's
`SHA256SUMS`, and installs it as `~/.local/bin/shard`. It needs no root. To check, repair, upgrade,
or uninstall shard on a host that setup prepared, run `shard setup` there again.

### Release binaries

To download by hand, use these assets from the
[GitHub releases](https://github.com/presmihaylov/shard/releases):

| Host | Asset |
|---|---|
| Linux x86-64 | `shard-linux-amd64` |
| macOS Apple silicon | `shard-darwin-arm64` |
| macOS Intel, client use only | `shard-darwin-amd64` |
| Checksums | `SHA256SUMS` |

Check the asset against `SHA256SUMS`, make it executable, and run it with `setup`:

```sh
chmod +x shard-linux-amd64
./shard-linux-amd64 setup
```

Setup installs that binary as `/usr/local/bin/shard`. On Linux it also installs `shard-init` from
the same release. The Mac binary already embeds its VM shim and guest supervisor.

### Build from source

You need Git, Make, and the Go version in [go.mod](go.mod):

```sh
git clone https://github.com/presmihaylov/shard.git
cd shard
make build-linux build-shard-init-linux
```

On a Mac, install the Xcode Command Line Tools and run `make build-darwin` instead. On Apple silicon
it produces `bin/shard-darwin-arm64`. On Linux, setup downloads `shard-init` from the release that
matches the binary's version, and it refuses when no such release exists. So a build from between
releases is for development only. See [the release guide](docs/release.md) for how a release is
built and verified.

## Providers

| Provider | Host | Isolation | Pause, resume, fork |
|---|---|---|---|
| gVisor (`gvisor`) | Linux | User-space kernel | Yes |
| Sysbox (`sysbox`) | Linux | User namespace | No |
| runc (`runc`) | Linux | Host kernel, no user namespace | No |
| Firecracker (`firecracker`) | Linux with `/dev/kvm` | MicroVM | Yes |
| Virtualization.framework (`vz`) | Apple silicon, macOS 14+ | VM | Yes |

Every provider supports create, exec, stop, start, remove, and filesystem snapshots. An unsupported
verb fails with an error that names the provider. shard never swaps in another mechanism.

`shard setup` recommends Firecracker when `/dev/kvm` is usable, gVisor on Linux without it, and `vz`
on a Mac. You have to pick Sysbox or runc yourself. **Sysbox is single-tenant. Use runc only for
code you trust.** Read the [full provider matrix](docs/provider.md) before you pick a provider.

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
sudo shard run --name job --memory 512MiB alpine:3.20 echo job done
sudo shard exec job echo sandbox still available
sudo shard stop job
sudo shard snapshot create --name job-files job
sudo shard create --name job-copy --memory 512MiB --snapshot job-files
sudo shard remove --force job-copy
sudo shard remove job
sudo shard snapshot remove job-files
```

Flags go before the image or sandbox name, and the command goes after it. `--memory` and `--disk`
take sizes such as `512MiB` or `2GiB`. `--vcpus` takes a whole number. To name a secret's
destination, use `--destination` or its alias `--dest`. Grant a secret only to destinations that
never return the credential in a response. See [the CLI reference](docs/cli.md),
[the lifecycle](docs/state-machine.md), [secrets](docs/secrets.md), and [egress](docs/egress.md).

## API and SDKs

The daemon serves a REST API over a Unix socket. For remote access, `shard serve` checks API tokens
and forwards requests to that socket. For public access, put an HTTPS proxy in front of it. The CLI
and the SDKs use `SHARD_REMOTE` and `SHARD_API_KEY` to connect. See
[the daemon and API guide](docs/daemon.md) and [the OpenAPI schema](docs/openapi.json).

The `useshards` SDKs live in [sdks/typescript](sdks/typescript) and [sdks/python](sdks/python). They
cover commands in a sandbox, files, the lifecycle, secrets, and policies. The TypeScript SDK needs
Node.js 20.3 or later. The Python SDK needs Python 3.11 or later and has both a synchronous and an
asynchronous client.

Install them with `npm install useshards` or `pip install useshards`. Their source and examples are
in those directories, and [the release guide](docs/release.md) explains how an SDK release is made.

## Documentation

- [Set up a host or a remote connection](docs/setup.md)
- [CLI commands and options](docs/cli.md)
- [Daemon, REST API, and remote access](docs/daemon.md)
- [Provider capabilities and limits](docs/provider.md)
- [Mac setup](docs/mac.md) and [the Mac provider](docs/provider-vz.md)
- [Lifecycle](docs/state-machine.md), [files](docs/files.md), [secrets](docs/secrets.md), and [egress](docs/egress.md)

## Status and contributions

**Pre-alpha.** The API, CLI, and SDKs can change without compatibility guarantees.

Read [AGENTS.md](AGENTS.md) for the code layout and contribution rules. Install the `golangci-lint`
version that [CI](.github/workflows/ci.yml) pins, then run `make check` before each commit. It runs
the format check, vet, lint, the unit tests, and the e2e script's own tests. Tests that need a
runtime, namespaces, or KVM carry the `integration` build tag and run on a host that has them. See
[the release guide](docs/release.md) for the platform checks.

## License

[Apache-2.0](LICENSE). See [NOTICE](NOTICE) for attribution.
