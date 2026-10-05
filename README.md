<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset=".github/assets/logo-dark.svg">
    <img alt="shard" src=".github/assets/logo-light.svg" width="300">
  </picture>
</p>

A single-node sandbox manager for Linux and macOS.

shard creates sandboxes from OCI images, runs commands in them, pauses and resumes their processes,
and forks live sandboxes. One binary is both the CLI and the daemon, and it drives five providers:
gVisor, Sysbox, runc, Firecracker, and Virtualization.framework.

## Why shard?

shard gives an agent a sandbox that outlives its commands. The agent's files stay between commands.
You can save its memory while it is idle, or fork its current state to try another path. The same
lifecycle commands work on a Linux server and on an Apple silicon Mac, and the host keeps control of
secrets and outbound network policy. shard manages one host. It does not schedule a fleet.

## Quickstart

Install the CLI, then set up this machine as a sandbox host:

```sh
curl -fsSL https://useshards.com/install | sh
shard setup
```

Create a sandbox, write a file, read it, and remove the sandbox:

```sh
sudo shard create --name demo --memory 512MiB alpine:3.20
sudo shard exec demo sh -c 'echo hello from shard > /tmp/hello.txt'
sudo shard exec demo cat /tmp/hello.txt
sudo shard remove --force demo
```

The third command prints `hello from shard`. On a Mac the daemon runs as your user, so drop `sudo`.

## Documentation

The docs live at [useshards.com/docs](https://useshards.com/docs): install and setup, providers, the
lifecycle, secrets and egress, the TypeScript and Python SDKs, and the CLI and REST API references.

## Status and contributions

**Pre-alpha.** The API, CLI, and SDKs can change without compatibility guarantees.

Read [AGENTS.md](AGENTS.md) for the code layout and contribution rules. Install the `golangci-lint`
version that [CI](.github/workflows/ci.yml) pins, then run `make check` before each commit. It runs
the format check, vet, lint, the unit tests, and the e2e script's own tests. Tests that need a
runtime, namespaces, or KVM carry the `integration` build tag and run on a host that has them. See
[the release guide](docs/release.md) for the platform checks.

## License

[Apache-2.0](LICENSE). See [NOTICE](NOTICE) for attribution.
