# shard
This is a work in progress, will announce when it's live and ready to be used!

A single-node sandbox manager. One binary runs isolated sandboxes on a Linux host, with or without
hardware virtualization, and gives them the same lifecycle verbs either way: run, exec, pause, resume
and fork. On a host with `/dev/kvm` it drives Firecracker microVMs; on a host without one it drives
gVisor. An optional `shard serve` exposes the same verbs over a self-hostable REST API.

**Status: pre-alpha.** Every verb runs on gVisor. Firecracker and the REST API do not exist yet.

## Sandboxes

```
shard create python:3.12 -- python -c 'print(1)'
```

It pulls the image, claims the record, allocates the network, creates the sandbox and starts the
entrypoint. Then it prints the id and returns. It never attaches: the entrypoint runs as the child
of `shard-init`, and the sandbox outlives it. `--env`, `--workdir`, `--user`, `--memory` and
`--cpus` shape the workload, and they go before the image.

`--user` sets the user of the entrypoint, never of the supervisor. PID 1 stays privileged so it can
always record how the entrypoint ended.

`SHARD_INIT_PATH` names the supervisor binary, and defaults to `/usr/local/bin/shard-init`.

`--secret NAME` hands the guest a placeholder for a stored secret as `$NAME`. The value stays on the
host, and the egress proxy puts it into a request on its way to the granted destination and nowhere
else. See `docs/secrets.md`.

`--policy NAME` names the egress policy the host enforces. Without one the sandbox reaches the
internet and nothing private; see `docs/egress.md`.

## Snapshots

Every sandbox sees at most one fixed CPU feature set, listed in `services/bundle/defaults.go`: what
Intel Broadwell, AMD Zen and everything newer have in common, and nothing a host may lack. That list
is the CPU bound on where a snapshot can restore. A host that lacks any listed feature runs a guest
with a smaller set and no error, and its snapshots restore only where that smaller set exists. gVisor
does not promise a restore across machines (gvisor#11486), so shard promises it only on the host
that took the snapshot, and treats anything else as best effort. Changing the list invalidates every
snapshot that exists, so it is not a thing to tune.

`shard pause` writes a running sandbox into a snapshot and frees its memory; `shard resume` runs it
again from there, and `shard fork` starts a new sandbox from the snapshot of another and leaves the
source as it is. A pause copies the writable layer, so it takes time and disk in proportion to what
the sandbox has written. The snapshot is the memory image plus a copy of the writable layer as it was at the
pause, so two forks of one snapshot share nothing and a resume does not consume it.

`shard clone` takes no memory at all: it copies every file a stopped or paused sandbox kept, `/tmp` included, and runs
its entrypoint again from the beginning, under a new id and address, the way `shard start` would
under the old one. Two clones of one source share nothing, and the source stays as it was. It refuses
a running source, because a running sandbox is still writing the layer it would copy.

Measured on the devbox, a 2 vCPU Hetzner Cloud box with no `/dev/kvm`, on an idle Alpine sandbox
of about 40 MiB resident: pause 0.19 to 0.24 s, resume 0.46 to 0.48 s, fork 0.43 to 0.51 s. E2B quotes
about 4 s per GiB to pause and about 1 s to resume, and those numbers carry a cloud round trip that
these do not, so they compare the mechanism and not the product. `docs/demo.cast` is the whole run
on that box, recorded with `make devbox-demo`; play it with `asciinema play docs/demo.cast`.

## Secrets

```
printf '%s' "$TOKEN" | shard secret set --to api.example.com API_TOKEN
shard secret ls
shard secret rm API_TOKEN
```

A secret is granted to a destination, never to a sandbox alone. The store keeps the value in one file
of mode 0600, `secret ls` never prints it, and `secret rm` refuses while a sandbox still holds the
placeholder. The egress proxy puts the value into a request on its way to the granted host and
nowhere else. `docs/secrets.md` says what this stops and what it does not.

## Egress

```
shard policy create --allow api.example.com --deny any locked
shard create --policy locked python:3.12 -- python agent.py
shard policy show locked
shard policy rm locked
shard logs --egress <id>
shard proxy
```

A sandbox with a policy or a secret sends its web traffic through the egress proxy, which the first
verb that needs it starts. `shard proxy` runs one in the foreground instead.

A policy is an ordered list of `allow` and `deny` rules over addresses, prefixes, names and
name suffixes, and what matches none of them is dropped. The host enforces it in netfilter, applies
it again after every restore, and applies a change to every live sandbox at once. `shard logs
--egress` prints every allow and deny with the rule that decided it. `docs/egress.md` has the rule
syntax and what a policy implies.

## Images

```
shard pull python:3.12       pull an image and unpack its rootfs
shard image ls               list the pulled images
shard image rm python:3.12   remove one, with the rootfs no other tag needs
```

Everything lands under `/var/lib/shard`, which `--root` overrides. An image is unpacked once per
digest, into a read-only rootfs that every sandbox built from it layers over. A tag shard already
holds is never re-resolved: `shard image rm` and pull again is how you ask for a newer one.

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
