# shard
This is a work in progress, will announce when it's live and ready to be used!

shard is a single-node sandbox manager. One binary runs isolated sandboxes on a Linux host or a Mac,
with or without hardware virtualization, and gives them the same lifecycle verbs either way: run,
exec, pause, resume and fork. It drives gVisor, Sysbox when you need Docker or systemd inside the
sandbox, Firecracker microVMs on a host with `/dev/kvm`, and Virtualization.framework on a Mac. A
resident `shard daemon` owns the state and serves it over a REST API on a unix socket. The CLI is a
thin client of that socket, and each command runs one verb.

**Status: pre-alpha.** Every verb runs on gVisor. Firecracker and `vz` on an Apple silicon Mac with
macOS 14 or later run every verb but fork, which comes to them with SHARD-462 and SHARD-463. Sysbox
and runc refuse pause, resume and fork, and run every other verb.
Every verb talks to the daemon, so the daemon must be up. See `docs/daemon.md`.

## Providers

`shard daemon --provider gvisor|sysbox|runc|vz|firecracker` picks the provider for the host.
Without it, a Linux host whose `/dev/kvm` opens runs Firecracker, one without runs gVisor, and a
Mac runs `vz`. Sysbox and runc run only when named. A root that holds records keeps the provider
that made them, and `shard info` prints the pick. The table is the short form of the full matrix in
`docs/provider.md`, which also has the Firecracker column:

| | gVisor (Linux default without `/dev/kvm`) | Sysbox | runc | vz (Mac default) |
|---|---|---|---|---|
| Isolation | user-space kernel | container with a user namespace | **none**: a container on the host kernel | a micro VM per sandbox |
| Syscall cost | high on file-heavy work | near native | near native | native, inside the VM |
| Docker or systemd inside | no | yes | no | no |
| pause, resume | yes | **no, refused by name** | **no, refused by name** | Apple silicon on macOS 14 or later |
| fork of a running sandbox | yes | **no, refused by name** | **no, refused by name** | **no, refused by name** until SHARD-463 |
| Tenancy | many tenants per host | **one tenant per host** | **one tenant per host**, code you trust | many tenants per host |

Sysbox CE gives every container the same uid range, so two Sysbox sandboxes are isolated from the
host but not from each other. Run one tenant per Sysbox host.

runc isolates nothing. Root in the guest is root on the host. shard never picks it by default, and
`--provider runc` is the only way to select it.

`vz` runs only on a Mac. It uses Virtualization.framework and needs neither Docker nor root. Start
with `docs/mac.md`, and read `docs/provider-vz.md` for the contract.

## Sandboxes

```
shard daemon
shard run --name lab python:3.12 python -c 'print(1)'
shard exec lab python --version
shard create --name idle python:3.12
```

Shard flags precede the image or sandbox reference. The command and its arguments follow the reference.
An optional `--` before the command still works.

`shard run` creates a sandbox and starts the command after the image as its app. The image's own
ENTRYPOINT and CMD never run. Run prints what the app writes, stdout and stderr interleaved, until
the restart policy ends, and then exits with the app's last code, or 128 plus the signal that ended
it. It exits 125 when shard itself fails. `-d` prints the id once the app starts and returns.
Ctrl+C stops the app and cancels its restarts, a second Ctrl+C kills it, and a third leaves with 130.
Before the sandbox is up, run waits for it, then stops the app, or kills it after a second Ctrl+C,
and exits 130. The sandbox stays `running` through all of it, until `shard stop`.

`shard create` takes no command. Only `shard-init` runs, and the sandbox stays up for `shard exec`.

`shard daemon` runs first, in a terminal of its own or as the systemd unit in `packaging/systemd`.
It owns the state. Every other verb is a client of its socket and fails fast when the daemon is not
running.

The daemon never binds TCP. A client on another host reaches it through `shard serve`, an
unprivileged process that speaks plain HTTP behind an HTTPS proxy such as Caddy, checks a bearer
token and passes the bytes to the socket. A script or a CI job exports
`SHARD_REMOTE=https://shard.example.com` and `SHARD_API_KEY`, the token that `shard tokens mint`
issues, and every verb goes through the proxy to the front. `--token-file <path>` and
`SHARD_TOKEN_FILE` are the alternatives. See `docs/daemon.md`. A Mac that shard does not support,
an Intel Mac or one on macOS 13, can run shard inside a Linux VM as a workaround, as `docs/mac.md`
describes.

On create or run, shard pulls the image, claims the record, allocates the network and creates the
sandbox. Run then starts the app as the child of `shard-init`, and the sandbox outlives it.
`--restart` starts the app again after it exits, and goes on `run` only. `--env`, `--workdir`,
`--user`, `--memory` and `--cpus` shape the workload, and they go before the image.
`--memory` and `--disk` take a whole size such as `512MiB` or `2GiB`: KiB, MiB and GiB are binary,
KB, MB and GB decimal. Only `0` goes without a unit. The API and the record keep MiB.

`--user` sets the user of the app and of every exec only. The supervisor stays privileged as PID 1,
so it can always record how the app ended.

`SHARD_INIT_PATH` names the supervisor binary and defaults to `/usr/local/bin/shard-init` on
Linux. A Mac daemon carries its own guest build of the supervisor and installs it under `<root>/vz`.

`--secret NAME` hands the guest a placeholder for a stored secret as `$NAME`. The value stays on the
host. The egress proxy puts it into a header of an HTTPS request on its way to the granted
destination. See `docs/secrets.md`.

`--policy NAME` names the egress policy the host enforces. Without one, the sandbox can reach the
internet but no private address. See `docs/egress.md`.

`shard cp ./app.conf <id>:/srv/` and `shard cp <id>:/srv/app.conf .` copy one file into or out of a
running sandbox. The copy is streamed and byte exact. A copy into the sandbox keeps the file mode and
is atomic in the guest, and `--user` names the user who writes and owns the file. A directory travels
as a tar, with its modes and symlinks. A copy out refuses any entry in it that would land outside
the destination.

## Snapshots

Every sandbox sees at most one fixed CPU feature set, listed in `services/bundle/defaults.go`. The
set is what Intel Broadwell, AMD Zen and every newer CPU have in common, and it leaves out anything a
host may lack. That list bounds where a snapshot can restore. A host that lacks a listed feature runs
its guests with a smaller set and reports no error, and its snapshots restore only where that smaller
set exists. gVisor does not promise a restore across machines (gvisor#11486), so shard promises a
restore only on the host that took the snapshot and treats any other host as best effort. Changing
the list invalidates every existing snapshot, so do not tune it.

The snapshot verbs work on gVisor, and on `vz` on an Apple silicon Mac with macOS 14 or later. On
Sysbox, on runc and on `vz` on any other Mac, each snapshot verb refuses by name and the sandbox
keeps running.

`shard pause` writes a running sandbox into a snapshot and frees its memory. `shard resume` runs it
again from that snapshot, and does not consume it. A pause copies the writable layer, so its time
and disk cost grow with what the sandbox has written. The snapshot is the memory image plus a copy
of the writable layer as it was at the pause.

`shard fork` starts a new sandbox from a running one. It freezes the source for a moment, captures
its memory and its writable layer, lets the same sandbox run on, and starts the new one from that
capture, never from an older snapshot. Each fork takes a capture of its own, so two forks share
nothing, and the capture is never a snapshot you can name. Only gVisor forks today (SHARD-457).

`shard clone` takes no memory image at all. It copies every file that a stopped or paused sandbox
kept, `/tmp` included, and runs the entrypoint again from the beginning under a new id and address,
as `shard start` would under the old ones. Two clones of one source share nothing, and the source
stays as it was. Clone refuses a running source, because a running sandbox is still writing the layer
that clone would copy.

Measured on the devbox, a 2 vCPU Hetzner Cloud box with no `/dev/kvm`, with an idle Alpine sandbox
of about 40 MiB resident: pause takes 0.19 to 0.24 s, and resume 0.46 to 0.48 s.
E2B quotes about 4 s per GiB to pause and about 1 s to resume. Those numbers include a cloud round
trip that these do not, so they compare the mechanism rather than the product.
`docs/demo.cast` is the whole run on that box, recorded with `make devbox-demo`. Play it with
`asciinema play docs/demo.cast`.

## Secrets

```
printf '%s' "$TOKEN" | shard secret set --to api.example.com API_TOKEN
shard secret ls
shard secret rm API_TOKEN
```

A secret is granted to a destination, never to a sandbox alone. The store keeps the value in one file
of mode 0600. `secret ls` never prints the value, and `secret rm` refuses while a sandbox still holds
the placeholder. `docs/secrets.md` says what this protects against and what it does not.

## Egress

```
shard policy create --allow api.example.com --deny any locked
shard run --policy locked python:3.12 python agent.py
shard policy show locked
shard policy rm locked
```

A policy is an ordered list of `allow` and `deny` rules over addresses, prefixes and names. Traffic
that matches no rule is dropped. On Linux the host enforces the policy in netfilter. It applies the
policy again after every restore, and applies a change to every live sandbox at once. On a Mac the
daemon's own userspace netstack enforces it and writes every drop to the sandbox's egress log.
`docs/egress.md` has the rule syntax and what a policy implies.

## Images

```
shard pull python:3.12       pull an image and unpack its rootfs
shard image ls               list the pulled images
shard image rm python:3.12   remove one, with the rootfs no other tag needs
```

Everything lands under `/var/lib/shard`, and `--root` overrides that. shard unpacks an image once
per digest, into a read-only rootfs that every sandbox built from it layers over. shard never
re-resolves a tag it already holds. To get a newer image for that tag, run `shard image rm` and pull
again.

## Development

`make check` runs the same gates as CI: format check, vet, lint and tests.
`make fmt` and `make lint-fix` apply the fixes that can be made automatically.
Linting needs [golangci-lint](https://golangci-lint.run/) v2 (`brew install golangci-lint`).

The code sits in three buckets: `models/` for domain structs, `pkg/` for thin drivers over external
things, and `services/` for business logic. `pkg/` never imports `models/`, and `depguard` enforces
that in CI. [AGENTS.md](AGENTS.md) has the full layout and the rules that go with it.

The Linux substrates do not run on macOS. `make test` stays green on a Mac. Anything that needs
`runsc`, netns or KVM sits behind the `integration` build tag and runs on a Linux box through
`make test-integration`. `make build-darwin` builds the Mac binary, and `docs/release.md` says what
runs where.

`make e2e-firecracker` drives the whole lifecycle on Firecracker. It runs only on demand, because it
needs `/dev/kvm`, which CI and the devbox do not have, so no gate calls it. To run it, rent a
bare-metal KVM box, run the target there as root, and destroy the box afterwards. `docs/provider.md`
says what it proves.

`CLAUDE.md` is a symlink to `AGENTS.md`, so one document serves every agent. A Windows checkout
needs `core.symlinks=true`.

## API stability

The module stays at `v0` until launch and makes no stability promise. The provider interface is
scheduled to change twice, once after each substrate is real.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
