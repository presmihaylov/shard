# The provider contract

There is one interface, `models.Provider`, and each substrate has one implementation of it. The
code is in `models/provider.go`. This page says what the signatures cannot.

## The substrates

`shard daemon --provider <name>` picks one substrate for the whole host, and every sandbox on that
host runs on it. Each record names the substrate that made it. A root that holds records refuses
any other `--provider`, because the other substrate has never heard of those sandboxes.

| | gVisor (`gvisor`) | Sysbox (`sysbox`) | runc (`runc`) | vz (`vz`) | Firecracker (`firecracker`) |
|---|---|---|---|---|---|
| Isolation | a user-space kernel, `runsc` | a Linux container, `sysbox-runc`, with a user namespace and virtualised `/proc` and `/sys` | **none**: a Linux container, `runc`, on the host kernel with no user namespace | a VM per sandbox on Virtualization.framework, one `shard-vz-shim` each | a microVM, needs `/dev/kvm` |
| Syscall cost | high on file-heavy work (`npm install`, `git clone`) | near native | near native | near native | near native |
| Docker inside | no | yes | no | yes | yes |
| systemd as PID 1 | no | no | no | no | no |
| Tenancy | many tenants on one host | **one tenant per host** (see the Sysbox section) | **one tenant per host**, and only code you trust | many tenants on one Mac | many tenants on one host |
| Exit code | host-verified, behind the sentry | **guest-attested**, and lost if PID 1 dies while the daemon is down or the daemon dies mid-stop (see the Sysbox section) | **guest-attested**, because guest root is host root | host-verified, behind the VM | host-verified, behind the VM |
| Status | every verb | every required verb, no snapshot verb | every required verb, no snapshot verb | every verb on Apple silicon with macOS 14+; no snapshot verb on 13 or on Intel | every verb |

The capability table uses the CLI names. The first row holds the required verbs. The other three
rows are the optional verbs: `Capabilities` reports each one, and the CLI refuses the ones a
substrate lacks.

| Verb | gVisor | Sysbox | runc | vz | Firecracker |
|---|---|---|---|---|---|
| `create`, `start`, `stop`, `rm`, `clone`, `exec`, `logs`, `inspect` | yes | yes | yes | yes | yes |
| `pause` | yes | **no** | **no** | Apple silicon on macOS 14+, **no** on 13 or on Intel | yes |
| `resume` | yes | **no** | **no** | Apple silicon on macOS 14+, **no** on 13 or on Intel | yes |
| `fork` | yes | **no** | **no** | Apple silicon on macOS 14+, **no** on 13 or on Intel | yes |

### What a host picks without --provider

`--provider` wins over the host. Over a root, it must name the substrate that made the root.
Without the flag, the root decides first and the host decides second. So the same command runs
unchanged on a box with hardware virtualization and on one without:

| The root and the host | The substrate | The reason |
|---|---|---|
| any root that holds records | what made them | `it made the records under <root>` |
| a root with none, beside a `<root>.xfs` image | Firecracker | `it made the data image <root>.xfs` |
| a root with none, on Linux, `/dev/kvm` opens | Firecracker | `/dev/kvm opens` |
| a root with none, on Linux, no `/dev/kvm` | gVisor | `no /dev/kvm` |
| a root with none, on Linux, `/dev/kvm` will not open | gVisor | `/dev/kvm does not open: <error>` |
| a root with none, on macOS | vz | `macOS runs virtual machines through Virtualization.framework` |

**A root keeps the substrate that made its records.** No other substrate can read them. So when a
daemon upgrades on a host where `/dev/kvm` has since appeared, it keeps running the sandboxes it
already has. A `--provider` that names another substrate is refused before the daemon touches the
root, and the refusal names both substrates. The records, or the data image, name the substrate to
pass instead.

**A root leaves room for the sockets under it.** A unix socket path holds at most 107 bytes on Linux
and 103 on macOS. A vz VM's sockets sit under `<root>/sandboxes/<id>/`. A Firecracker vmm's sockets
sit in its jail, `<root>/jail/firecracker/<id>/root/`. So at start the daemon refuses a root longer
than 51 bytes for Firecracker, 58 for vz, or 96 for the container substrates (92 on macOS). The
refusal names the longest root that fits (SHARD-358, SHARD-306).

**A daemon upgrade keeps the output of a running microVM, and a downgrade does not.** A guest booted
before the logs protocol names no version in its state and sends raw output. A newer daemon lands
every byte of that output as it comes, with no resume. A newer guest under an older daemon is
unsupported. That daemon lands the guest's opening offsets as output and never answers them, so no
output follows.

**An unmounted data image still names Firecracker.** Only Firecracker puts an xfs image beside a
root, and every record lives inside that image. So a root whose image is not mounted looks empty
from outside. The image itself is what marks the root as Firecracker's. The daemon then mounts the
image back, and the records return. Without this rule, a host that lost `/dev/kvm` would start fresh
over the mountpoint and hide the records.

**The probe opens `/dev/kvm` instead of calling stat on it.** A node that this daemon cannot open
runs no microVM. So when the node is present but unusable, the pick stays at gVisor instead of every
create failing.

**Sysbox and runc are never picked for a host.** Sysbox is single-tenant, and runc is a plain
container on the host kernel. Only `--provider`, or a root they already made records under, can name
either one.

`shard info` prints the substrate and the reason. It takes no flags, and it asks the host and the
root instead of the socket. So it answers before a daemon exists, and it says what a daemon started
now with no `--provider` would run. `shard daemon status` says what the daemon that is already up
runs on. The two differ when that daemon was started with `--provider`.

```
$ shard info
provider   firecracker
reason     /dev/kvm opens
```

### systemd is not a sandbox's init

**systemd cannot run as a sandbox's PID 1, on any provider.** `shard-init` is PID 1 in every
sandbox and starts the command given at create as its child (`cmd/shard-init` uses `ForkExec`), so
the entrypoint is never PID 1. The image's own ENTRYPOINT and CMD never run, and with no command
`shard-init` runs alone. systemd refuses the system-manager role when it is not PID 1. It prints
"Explicit --user argument required to run as user manager." and exits. `systemctl is-system-running`
then reports `offline`, and the record stays `running` because a sandbox outlives its entrypoint.
The limit is not specific to Sysbox. It holds on gVisor and Firecracker too, because `shard-init` is
PID 1 in every sandbox. Docker inside a sandbox is unaffected. It works end to end on Sysbox, and on
`vz`, where `dockerd` runs as the entrypoint of a VM (SHARD-247, `docs/provider-vz.md`). To run
systemd, run it inside a Docker container that the sandbox starts. It cannot be the sandbox's own
init.

### What Sysbox does not do

**Sysbox has no pause, no resume and no fork.** `sysbox-runc` dropped upstream `runc`'s
`checkpoint` and `restore` (nestybox/sysbox#715, open since 2023), so there is no memory image to
take. The provider claims `{Pause: false, Resume: false, Fork: false}`, and each verb refuses by
name, for example `provider sysbox does not support pause on this host`. Nothing is emulated. A
`pause` on Sysbox is a refusal and not a stop, and the sandbox keeps running. `clone` still works,
because it needs no snapshot.

**Sysbox CE is single-tenant.** Sysbox CE maps every container to the same host uid range,
`0 165536 65536`. Exclusive ranges were a Sysbox EE feature, and EE is gone. So two Sysbox
sandboxes are isolated from the host but **not from each other**. A process that escapes one
container's namespaces has the uid of every other sandbox's root. Run one tenant per Sysbox host,
and do not place two customers' sandboxes on one. shard pins that same mapping on the user namespace
that owns each sandbox's netns (`services/provider/sysbox.Userns`). That is why the guest holds
`CAP_NET_ADMIN` over its own interface and nothing else. The upstream fix is an exclusive-range
allocator in `sysbox-mgr`, and this limit stands until it lands.

**A Sysbox guest gets no kernel keyring.** The keyring quota is per host uid, and every Sysbox guest
root is host uid 165536. So one guest that filled the quota would make every later create fail
(SHARD-367). Each bundle carries a seccomp filter that answers `add_key`, `keyctl` and
`request_key` with `ENOSYS`, and `sysbox-runc create` runs with `--no-new-keyring`. The filter is a
deny-list, because `sysbox-runc` rewrites an allow-list to admit those three calls again. It
answers `ENOSYS` and not `EPERM` because `runc` inside the guest skips its session keyring on
`ENOSYS`, so nested Docker still starts.

**Sysbox does not verify the entrypoint's exit code.** `shard-init` reports the exit record on its
fd 0, which the host holds. It also clears its dumpable flag, which puts `/proc/1/fd` out of the
guest's reach. On gVisor that protection holds. The sentry enforces the capability set the bundle
grants, which has no `CAP_SYS_PTRACE`, so a guest write to `/proc/1/fd/0` fails with `EACCES`.
`sysbox-runc` gives guest root the full capability set whatever the bundle lists, `CAP_SYS_PTRACE`
included. So guest root controls PID 1 and the entrypoint. It can write a forged record, drive the
exit value or pick the signal, and a background write after the real exit makes `inspect` report
the forged code. No channel on Sysbox is readable by the host and unwritable by the guest, so no
mechanism can fix this. The exit code of a Sysbox sandbox is what its root attests, and on a
single-tenant host that is your own code. The channel is a memfd of 4 KiB, sealed against growing
and shrinking, so no guest write can fill the host's disk or memory. After a restart the daemon
finds the channel again through fd 0 of PID 1, confirmed in the sandbox cgroup. It takes the channel
only while it is a regular file of 4 KiB with its seals and the inode that create recorded.
`inspect` names anything else as `exit channel replaced`. A stop copies the record before it
returns. If PID 1 dies while the daemon is down, or the daemon dies inside the stop that ended PID 1,
the unread exit record is lost with it. Firecracker verifies the exit code behind the VM boundary
the way gVisor does behind the sentry.

**Sysbox runs where `sysbox-runc` runs.** It needs root, the Sysbox package installed on the host,
and a kernel that Sysbox supports. There is no fallback to gVisor. On a host without `sysbox-runc`,
the daemon answers the reads and refuses every sandbox verb, as it does on a gVisor host without
`runsc`.

### What runc does not do

**runc isolates nothing.** The guest shares the host kernel, with only namespaces and cgroups
between them, and root in the guest is root on the host. Use it for code you trust, or on a box you
can lose. It is never picked by default, and `--provider runc` is the only way onto it. It has no
pause, no resume and no fork, because shard drives no checkpoint on it. Each verb refuses by name,
for example `provider runc does
not support pause on this host`. It runs where the `runc` package is installed. It needs root, and
there is no fallback to gVisor.

**runc carries Docker's default seccomp profile.** The profile answers the keyring calls with
`EPERM`, and `runc create` runs with `--no-new-keyring`, so a guest spends none of the host's
keyring quota.

**runc carries Docker's AppArmor profile where the module is on.** `shard-default` is Docker's
`docker-default` under a shard name, copied rule for rule from `moby/profiles/apparmor`. Because
the name differs, it never replaces the profile that Docker itself loads on the same host. Every
runc sandbox runs under it. On a host where the module is on but `apparmor_parser` is missing,
every sandbox would run unconfined. So the daemon refuses the runc provider there and names the
`apparmor` package. A host with the module off runs no profile, as Docker does.

## Required verbs against optional verbs

Fourteen verbs are required. Every substrate must do all of them, and none of them has a capability
flag. They are `CheckResources`, `Create`, `Start`, `Stop`, `Remove`, `Clone`, `Exec`, `Signal`, `Wait`,
`ExitStatus`, `Status`, `Restarts`, `LogPath`, and `Capabilities` itself.

`CheckResources` answers whether the substrate can run under a bound before the orchestrator writes
a record, so a refusal leaves nothing in `ls`. Only vz and Firecracker refuse anything. A VM's
memory is real memory, so both refuse `--memory 0` and a bound under 128 MiB by name, and a `--disk`
whose last block group cannot hold its own metadata. Firecracker also refuses a `--cpus` above 32
and a `--disk` under 11 MiB. gVisor, Sysbox and runc take every bound. `Create` checks its spec
again, so a clone or a fork is held to the same rule.

`Clone` is required because it needs nothing a substrate may lack. It copies the writable layer
that another sandbox kept, and runs that sandbox's entrypoint again from the beginning, under the
new id and the new network. It refuses a source that is alive or still mounted. Of the source, it
reads only the status and the state directory. Firecracker reflinks `overlay.raw` where gVisor
copies an overlay layer, so the clone shares the source's blocks. A root whose filesystem cannot
share blocks (ext4, tmpfs) refuses the clone by name instead of copying every byte. XFS and Btrfs
can.

Three verbs are optional: `Pause`, `Resume` and `Fork`. `Capabilities` reports one boolean per
optional verb, and it is the only place where one substrate may differ from another.
`fork` takes a paused source on every substrate. The snapshot is what the pause wrote. A resume runs
on past the snapshot and keeps its name. So a running or stopped source is refused, even when its
record still names a snapshot.

### What vz does and does not do

`vz` is the substrate `shard daemon` picks on a Mac when `--provider` is empty, and it is refused by
name anywhere else. Each sandbox is one Virtualization.framework VM. The VM boots shard's own arm64
kernel over an APFS clone of the image's ext4 disk. `services/kernel` fetches the kernel release
once under the root, and `SHARD_KERNEL` and `SHARD_KERNEL_SHA256` override it. One `shard-vz-shim`
holds each VM. `make build-darwin` embeds the shim, and the daemon signs a copy of it under
`<root>/vz`. That build also embeds the static linux `shard-init` for this Mac's arch, and the
daemon installs it under `<root>/vz` as the initrd's `/init`. A `go build` alone has neither, and
the first sandbox says so. `SHARD_INIT_PATH` names a guest `shard-init` of your own instead.
`--memory` is required and is a hard cap, and `0` is refused by name. There is no bridge. The
daemon leases each guest an address from the pool and terminates its frames in a userspace stack
that answers for the gateway alone. It serves the proxy and the resolver on that stack. The stack's
own NAT table sends a guest's port 80 and 443 to the proxy wherever the guest dialed them, as the
host chains do on Linux. Every other TCP or UDP flow is judged by the same compiled chains that the
host ruleset is built from, so a policy means the same on both hosts (SHARD-246). The stack drops a
refused flow and writes it to the sandbox's egress log, which `docs/provider-vz.md` covers.
`pause`, `resume` and `fork` are one VZ save and a restore, which macOS 14 added on Apple silicon.
On 13, and on an Intel Mac, all three refuse by name. Every restore of one save wakes with the same
guest crng key. So each `resume` and `fork` sends the guest 32 bytes of host entropy, and
`shard-init` rekeys from them before the verb returns (SHARD-293). The guest's processes are still
frozen from the pause when the seed lands, and they thaw only after it, so no copy reads a
`/dev/urandom` byte of the saved key (SHARD-310). The three resource bounds below hold on the Linux
substrates. `vz` and `firecracker` have no host cgroup, and each section says what the VM does
instead.

### What Firecracker does and does not do

`firecracker` is `--provider firecracker`. It runs on a Linux host with `/dev/kvm` and with the
`firecracker` and `jailer` binaries on PATH. Each sandbox is one `firecracker` process, driven over
its API socket in its jail. Each one boots shard's own amd64 kernel from the parts that the section
below describes: the image's EROFS file read-only, the sandbox's `overlay.raw`, and an initrd of the
static `shard-init` at `SHARD_INIT_PATH`, which the daemon writes once under `<root>/firecracker`.
`services/kernel` fetches the kernel release once under the root, and `SHARD_KERNEL` and
`SHARD_KERNEL_SHA256` override it. The host needs `erofs-utils` for the pull. The host speaks to
the guest over vsock alone, through the socket that firecracker proxies it on, so `exec`, `logs`
and the exit arrive the way they do on `vz`. The vmm has no stop of its own. A stop tells
`shard-init` to end the entrypoint and reboot, because a reboot is the one guest exit that makes
firecracker end its process (a power off leaves it running). When the grace runs out, the stop
kills the process. A guest that the host can no longer reach over vsock is still a running VM.
`inspect` says so, and `stop` kills it without waiting out a grace that the guest could not hear. A
daemon restart adopts a running vmm by its socket. It also resumes a vmm that an interrupted `pause`
left paused, because that stopped guest would answer no handshake. A vmm that does not answer that
adopt within 4 s reads `unresponsive` with its pid. It keeps running, because a thawed vmm gives
back the same VM (SHARD-392). A vmm that the daemon holds reads `unresponsive` the same way when it
misses a probe of 4 s, and a later answer reads `running` again (SHARD-439). `--memory` is required,
`0` is refused by name, and 128 MiB is the least a guest boots with. The guest's network is a veth
on the same bridge that the other substrates use. Its host end is named `shardv<n>`, and it is a
port under the same host rules. The anti-spoof pair, the IPv6 drop and the egress chain key on that
name. The proxy redirect keys on the leased address, and the private floor keys on the bridge. So a
policy reads and logs the same on every substrate. The other end of the veth is in the sandbox's own
namespace. There a bridge with no address joins it to a tap that is also named `shardv<n>`, so the
vmm shares no network namespace with the host (SHARD-431). The vmm opens the tap as the guest's
`eth0`, with a MAC derived from the lease. Once the guest is up, and before the entrypoint runs,
`shard-init` takes the address, the gateway and the resolver over vsock. The next start after a stop
leases the same address and builds the namespace and the tap again for the new vmm. `rm` releases
both.

The daemon spawns every vmm through Firecracker's `jailer`, and never as root (SHARD-306). Each
sandbox gets a uid of its own, and a gid with the same value, from 0x70000000 to 0x7FFDFFFF
(1879048192 to 2147352575). That range is clear of Sysbox, the `/etc/subuid` defaults and the
systemd ranges. A counter in `<root>/firecracker/next-uid` hands out each uid once. A restart and a
resume keep the uid, and a clone and a fork get a new one. The jailer puts itself in the sandbox's
cgroup and network namespace. It then starts the vmm in new pid and mount namespaces, chrooted into
`<root>/jail/firecracker/<id>/root`, with no capability and under its seccomp filter. The jail holds
the kernel, the initrd and the image. On a restore it also holds the snapshot's state and memory.
Each of these is a reflinked copy that only the uid can read. The overlay is a hard link that the
uid can write, so the guest writes where `clone` and `pause` read. The tap goes to the uid too, so
the vmm can open it with no capability. Every spawn gets a fresh jail, and every end of a vmm
removes it. The jailer makes `/dev/kvm` in the jail and runs the vmm from there, so at start the
daemon refuses a root on a `nodev` or `noexec` mount. The jailer gets no `--resource-limit`. Its
default of 2048 open files outlasts the vsock muxer's cap of 1023 connections, and those
connections are the one count of descriptors that a sandbox grows. A vmm that a daemon spawned
before the jail existed is still adopted at its socket in the state directory, and its next start
jails it. A vmm from before SHARD-431 keeps its host tap until its stop, and its next start or
resume joins a namespace.

`pause` freezes the guest and stops the vCPUs. It writes the vmm's state and the guest's
memory into the snapshot directory, beside a reflinked copy of `overlay.raw`. It then marks the
snapshot complete and ends the vmm. It stages all of that beside the snapshot that the directory
already holds, and swaps the two in one step, so no interruption leaves the sandbox with neither.
The record stays, so `inspect` reports the sandbox stopped, and the snapshot is what brings it
back. `resume` and `fork` each load that snapshot into a fresh vmm, over its own reflinked copy of
the overlay and a reflinked copy of the memory file in its jail. Firecracker maps the memory
private, so N sandboxes share the blocks on disk and none of them writes to them. Neither verb
consumes a snapshot. You can fork as many sandboxes from one snapshot as you like, each on its own
writable disk, and the source and the snapshot stay untouched. A fork restores with the source's
address, MAC and hostname. `shard-init` replaces all three in place over vsock before the guest
does anything else. The interface goes down for the MAC change, which is why a fork's frames reach
the bridge under its own MAC and not the source's. Firecracker cannot pause the wall clock, so the
guest's clock is corrected at the load on x86_64, where it reads kvm-clock, and nowhere else. Every
load of one snapshot also wakes with the same guest crng key, and the kernel has no vmgenid driver.
So each `resume` and `fork` sends the guest 32 bytes of host entropy, and `shard-init` rekeys from
them before the verb returns (SHARD-266). A restore keeps a marker until the seed lands. If the
daemon is interrupted in between, it reseeds the guest it adopts, and it ends a guest that refuses
the reseed or the thaw. No guest process draws from the saved key in between, because the snapshot
holds the guest frozen. `pause` has `shard-init` freeze the sandbox cgroup and then the root's
writes before it stops the vCPUs, and every restore reseeds the guest before it thaws it
(SHARD-409). The root is an overlay, which takes no `FIFREEZE`, so the freeze holds its ext4 upper
disk instead. A guest that cannot freeze refuses the pause, and the VM runs on. The thaw paths are
the ones that `docs/provider-vz.md` lists for `vz`: a failed pause, a daemon interrupted between the
freeze and the snapshot, and a control connection that dropped with the freeze's answer. A VM
booted by an older shard runs a `shard-init` whose freeze cannot reach the upper disk. Its state
says so, and the pause is refused before any freeze, with an error that says to restart the sandbox
first.

A `pause` takes a Firecracker Diff snapshot, which writes only the pages that the guest holds in
memory (SHARD-450, SHARD-451). After a boot, the pages that the guest never touched stay holes in a
sparse file, and a hole reads as zero. After a restore, shard first reflinks the memory that the vmm
loaded to the snapshot path, and firecracker merges the Diff into that copy, never into the file
that it maps. So a pause reads nothing back from the old snapshot. Firecracker finds the pages with
mincore(2), which over-counts: a page that the guest only read after a restore is resident too, so
the pause writes it again, unchanged (SHARD-458). mincore also misses a page that the host swapped
out. So a pause refuses a vmm whose cgroup it cannot hold at `memory.swap.max` 0, and names the
cgroup; every boot sets that value. Only firecracker 1.13.0 and newer take a Diff this way, so the
daemon refuses an older `firecracker` at its start, and names its version. Diff snapshots are a
developer preview in Firecracker, so an upgrade of the binary must pass the memory-integrity kit of
SHARD-450 again before it ships.

Two more limits apply. The data dir must be able to clone a file by sharing its blocks, because
`fork` on this provider needs that, as `docs/daemon.md` covers. The daemon probes its root and puts
a loopback XFS under a root that cannot, so no `pause` ever fails halfway for that reason. The other
limit is that the vmm's state names each drive by its path in the jail, so a load opens the fork's
own `overlay.raw`. A snapshot taken before the jail existed names host paths instead, and `resume`
and `fork` refuse it. To recover, start the sandbox and pause it again.

Every writable drive runs with the cache type `Writeback`, so a guest `fsync` returns only once the
vmm has flushed the data to the host disk. The firecracker default, `Unsafe`, drops the flush. The
read-only EROFS base keeps the default. The vmm fixes the cache type when it boots, and a snapshot
keeps the type that its vmm ran with. So a sandbox created before the release that set this cache
type, and any snapshot taken before it, keep `Unsafe`. A daemon restart adopts the running vmm as
it is, and a `resume` or a `fork` of such a snapshot loads its saved drive. A `stop` and a `start`
boot a fresh vmm with `Writeback`, which gives that sandbox a durable `fsync`. The daemon never
stops a sandbox to make that switch.

`scripts/e2e-fc.sh`, behind `make e2e-firecracker`, drives the whole lifecycle on this provider. It
starts the daemon over a root that it turns into an XFS image, and runs `create` with `--memory`. It
checks the vmm's jail, uid and seccomp filter, its host cgroup and its bounds, then `logs`, `exec`
and an entrypoint that exits. One guest outgrows its memory and comes back once under
`--restart-on-oom`, and another fills up on every boot and spends `--restart-on-oom=2`. The run
then checks the policy and the proxy on the vmm's link, a daemon restart that adopts the vmm, and a
vmm lost while the daemon was down. After that come `pause`, a `fork` of the paused snapshot,
`resume`, `stop` with the cgroup kept empty, two clones by reflink, `start` back into that cgroup,
and `rm`. The last check is a host with no link, no namespace, no vmm, no jail, no cgroup, no image
and no fstab line left. It runs on demand only. It needs `/dev/kvm`, which no CI runner and no
cloud devbox has, so CI, `make check`, `make e2e` and `make devbox-e2e` never call it. To run it,
rent a bare-metal KVM box, run `sudo make e2e-firecracker` there with `erofs-utils`, `xfsprogs`,
`firecracker`, `jailer` and Go on it, and destroy the box. `SHARD_KERNEL` and `SHARD_KERNEL_SHA256`
point the run at a kernel on the box. When they are unset, the daemon fetches the release.

The guest reaches the resolver and the proxy on the bridge address. So a host firewall that drops
`INPUT` discards those packets after shard's own table has accepted them. A rented box with `ufw` on
is the common case. Before the suite, run `iptables -I INPUT -i shard0 -j ACCEPT` there, or
`ufw allow in on shard0`. The host check fails by name when the policy is `DROP` and no such rule
exists.

`SHARD_ROOT` is where a run keeps its state, `/var/lib/shard-fc-e2e` by default. The daemon mounts
the XFS image over it. The image takes half the free space of the disk under the root, at most
100 GiB, so that disk needs 20 GiB free. `MEMORY` is the `--memory` of every create, 256 MiB by
default. The run deletes its root and the image beside it. So it refuses a `SHARD_ROOT` that already
holds files, unless the marker file `<root>.e2e-owned` names that root as one of its own. Point it
at an empty or absent path.

The bridge `shard0` and the nft tables `inet shard` and `bridge shard` are host-wide. They form one
set for the whole box, not one per root. So the teardown drops all three, and the last step proves
they are gone. Two runs on one box therefore collide over them. Run this suite and any other e2e on
the same box one at a time. The run refuses to start while any `shard` daemon is on the box,
because the teardown would take that daemon's bridge and policy with it.

## Refuse, never downgrade

A provider that cannot do an optional verb returns `models.Unsupported(provider, verb)`. That error
names both the provider and the verb, and it unwraps to `models.ErrUnsupported`. The CLI asks
`Capabilities` first and refuses before it holds or claims anything, so the provider's own refusal
is the last line of defence. A provider never falls back to a weaker mechanism, and it never returns
a nil error after doing something else.

The sentinel is reserved for that one meaning. A missing kernel feature on a developer Mac is not a
refused verb, so `bundle`'s overlay stub and `netns.ErrNotLinux` are plain errors.

`Capabilities` is computed once, in the constructor, so the method is a cheap getter. A probe that
needed a context and an error would be a fourth thing to get wrong.

## What a memory bound means

`--memory` bounds a sandbox the same way on every substrate. Past the bound the whole sandbox dies,
and not just one process inside it. The daemon restarts the sandbox when the record set
`restart_on_oom`. `create` refuses by name a bound above the host's total memory (`MemTotal` on
Linux, `hw.memsize` on a Mac). Such a bound never binds, because the host OOM killer acts first. A
bound of the host's whole memory is still accepted, so leaving room for the host is the operator's
call.

gVisor sets `memory.oom.group=1` and `memory.swap.max=0` on the host cgroup, and Sysbox and runc set
the same pair. `sysbox-runc` and `runc` apply `memory.max` from the bundle but neither of the two
knobs. Without them the OOM killer would take one guest process, the sandbox would live on, and
`oom_restarts` would stay at zero.
On `vz` the bound is the VM's memory, and `shard-init` puts the same pair on a cgroup inside the
guest. There `memory.max` is the VM's memory less 32 MB of headroom for the kernel and `shard-init`
itself, with `memory.oom.group=1` and `memory.swap.max=0`. `shard-init` unshares a cgroup namespace
rooted at that cgroup, then moves itself to a sibling, `init`. That way a pause can freeze every
guest process and leave the supervisor free to answer. Each process the supervisor starts is born
into the bounded cgroup by `CLONE_INTO_CGROUP`. So everything a guest starts, a Docker daemon and
its containers included, lands under the bound. `shard-init` alone is exempt from the killer,
through `oom_score_adj=-1000`. Each child it forks runs `shard-init -expose` first, which gives the
exemption up before the workload can fork. When the killer takes the group, `shard-init` reads
`memory.events.local` and reports the kill over vsock instead of an exit. The guest then holds
that state and does not power off on its own. The host writes the `oom` marker first, and only then
sends the stop. So a daemon that dies between the report and the marker finds the kill again in the
state that the next connection replays, and marks it then. Once the marker is down, `Status` says
`OOMKilled`, no exit record lands, and the daemon restarts the sandbox on `restart_on_oom` exactly
as it does on Linux. A kill that finds no host attached, during a reconnect after a sleep, waits
the same way for that replay. The bound needs room under the headroom, so `vz` refuses a
`--memory` below 128 MiB by name.
On `firecracker` the guest half is the same, down to the marker and the replay, because the guest
runs the same `shard-init`. A host cgroup alone would see nothing, since the vmm holds the guest's
whole memory from the boot and its host footprint stays flat while the guest fills up. The host
adds a cgroup of its own around the vmm, at `/sys/fs/cgroup/shard/<id>`. Its `memory.max` is the
guest's memory plus 64 MiB for what firecracker holds beside it, with the same
`memory.oom.group=1` and `memory.swap.max=0`. The vmm joins it between the spawn and the API call
that configures the machine. cgroup v2 charges a process only for the pages it faults after a move,
so a vmm moved in later would leave the guest's memory charged to the parent. That ceiling is for
host safety and is never the trigger. It sits above the guest's whole memory, so the guest's own
killer runs first on every workload, and the daemon hears an OOM instead of a death.

## What a cpu bound means

`--cpus` bounds a sandbox to a share of the host CPUs, the same way on every substrate. `--cpus 0`,
the default, sets no bound. `cpu.max` stays `max`, and the sandbox runs on every host CPU. A
positive `N` caps it at `N` CPUs of run time, as a `cpu.max` quota of `N * 100000` over a `100000`
period. `shard create` refuses a negative value with an error, because a bound below zero is not a
way to write unbounded. It refuses by name a value above the CPUs the daemon may run on, because
such a quota never binds, and a large enough one overflows to no bound at all. It refuses a fraction
such as `0.5` with an error that names it, because a VM gets whole CPUs and a rounded bound is not
the one that was asked for. On `vz` the count is the VM's virtual CPUs and not a quota. `--cpus 0`
gives the VM one virtual CPU per host CPU, held inside the framework's ceiling. A positive `N` gives
it `N`, and a value outside the host's range is refused.

## What the pids bound means

Every sandbox cgroup holds at most `4096` processes. Unlike the memory and cpu bounds, this bound is
never off and cannot be adjusted. There is no flag and no field. `pids.max` is written at create and
again on every launch, so a sandbox created before the bound existed is capped when it runs again.
`4096` leaves headroom for systemd, Docker and nested containers, yet stops a fork bomb far below
the host PID count. The bound is fixed because on Sysbox a guest process is a host process, so an
unbounded fork bomb in one sandbox takes the host down. On gVisor the bomb stays in the sentry and
hits `memory.max` first, but the same bound applies. On `vz` there is no bound. A guest process is a
process inside the VM, so a fork bomb stays there and hits the VM's memory, and the host is
untouched.

## What a disk bound means

`--disk` bounds everything a sandbox can write, the same way on every substrate. The writable layer
over the image, `/tmp` and the supervisor's files under `/.shard` all sit on one ext4 image per
sandbox. That image is `disk.img` in the sandbox's state directory, and the daemon loop-mounts it at
`disk/` before the overlay stacks on it. The bound is never off. `--disk 0`, the default, means
`10240` MiB, and a positive `N` overrides it. The image is sparse, so an unwritten sandbox costs the
host nothing. It is truncated to the bound, so host usage stops there whatever the guest does.
Inside the guest a write past the bound fails with `ENOSPC`, and the sandbox lives on. `df` shows
the bound less what ext4 keeps for itself.

A stop detaches the disk and a start mounts it again, so the layer survives a stop and the next
start. On Sysbox the disk stays mounted while `sysbox-runc` holds the stopped sandbox, because
`sysbox-mgr` chowns the upper layer back when the container is deleted, at the next start or at
`rm`, and it must find the layer in place. Fork and clone copy the layers into a disk of their own,
bounded the way the source was, and config.json carries the bound for that. The record carries the
resolved bound, so `inspect` shows the value the image enforces instead of a bare `0`.
`shard create` refuses a negative value. On the VM providers it also refuses, before the record
exists, a bound that `ext4.Grow` cannot reach, and it names the nearest sizes that it can reach.
The bounds it cannot reach are anything under 7 MiB on Firecracker, which is the empty overlay's
own size, and `N*128+1` or `N*128+2` MiB on both providers, where the last 128 MiB block group is
too small for its own metadata. On `vz` a bound under the image's own disk fails after the pull,
because only then is that size known (SHARD-280). The host needs `mkfs.ext4`, which `e2fsprogs`
ships. On Sysbox the directories that `sysbox-runc` backs from the host, `/var/lib/docker` among
them, sit outside the image and so outside the bound.

## What a microVM boots from

A microVM provider, Firecracker first, boots no OCI bundle and no ext4 root disk. It boots a
read-only EROFS image of the pulled image, an `overlay.raw` of its own, and an initrd. The image
service builds the EROFS image once per digest under `images/erofs/<digest>.erofs`, from the same
unpacked tree that the bundle path uses. It runs `mkfs.erofs` at 4 KiB blocks, so an image built on
a 16 KiB-page host mounts in a 4 KiB-page guest. The host needs `erofs-utils`. `overlay.raw` is one
empty ext4 image per sandbox, written in Go and grown to the disk bound. So the bound above holds
the same way, because every write of the sandbox lands on that image and stops there. The initrd is
`shard-init` alone, as `/init` of a newc archive, and it is the same file that the vz provider
boots. The kernel command line hands it `-base` and `-overlay`, the two block devices, and
`-console` for where its stderr goes. `shard-init` mounts the EROFS image read-only and the ext4
disk over it, and lays `upper` and `work` on that disk. It then mounts the overlay as the root,
moves the kernel filesystems across and pivots onto it, exactly as the one-disk `-root` boot does.
It stays PID 1, and the host sends the entrypoint over vsock as it does for the one-disk boot. With
no command the host sends an empty one, and the guest reports ready and forks nothing. `vz`
keeps its ext4 root disk, and an APFS clone is its overlay. The guest kernel must carry
`CONFIG_EROFS_FS` and `CONFIG_OVERLAY_FS`. Both shipped kernel configs, amd64 and arm64, set both
(SHARD-265). On a guest kernel that lacks the first, the boot fails at the base mount, and the
console log says so.

## What `Status` means

`Status` asks the substrate and reports what it says now. It never reads the shard record, and the
record never answers for it. The two disagree on purpose in these cases:

- A record that says `running` can outlive a `shard` restart, or a sandbox that someone killed by
  hand.
- A sandbox stopped before its entrypoint ran leaves nothing at the substrate. The record says
  `stopped`, and `Status` reports `Exists: false`.

`Status.Alive()` is `Exists && State != stopped`. Only `Stop` and `Pause` take a sandbox out of that
state. A gVisor `Pause` that breaks off after its checkpoint began loses the sandbox, because the
sentry exits after any checkpoint, whether the checkpoint was taken or not. The provider returns
`models.LostError`, and the record ends `failed` (SHARD-336). runsc never probes a paused sandbox,
so `Status` reads a paused sandbox whose sentry is gone as `stopped`. `stop` and `rm --force` then
end it. The exit of the entrypoint is not a transition, and the return of `Wait` does not end
anything. Under a restart policy, `Wait` returns the first exit of the run and not the settled one.
The supervisor rewrites the exit file on each exit and clears nothing, so only a stopped sandbox
answers with its last exit. No verb waits on the entrypoint. `stop` is the one caller, and it reads
only after the sandbox is down.

## What `Exec` means

`Exec` runs a second process in a sandbox that is already running. That process is never the
entrypoint. The supervisor does not see it, its exit ends nothing, and a signal that ends it never
reaches the sandbox. Only `Stop` ends a sandbox.

It reports the command's own exit code, which `Create` and `Start` do not, and that is what makes
the verb useful. An error means the exec never ran, and an exit code means it did.

An empty `ExecSpec.User` means the user the entrypoint runs as, which is how `docker exec` inherits
it. The supervisor's own process runs as root. So the entrypoint's user is recorded as the
`-user uid:gid` in the supervisor's argv, and the provider reads it back from the sandbox.

`ExecSpec` carries `*os.File` instead of `io.Reader`, because a TTY is one pty replica that the
caller allocates on the host, and a pipe cannot be one.

## Who owns what

- The provider owns the whole layout inside `StateDir`. Nothing outside the provider may name a file
  there.
- The repository owns the record and the directory itself. It removes the directory only after
  `Remove` has dropped every mount inside it.
- The network service owns the namespace, the address and the host interface. `NetworkSpec` is
  allocated before `Create`, so a provider on Linux joins a namespace that it did not build, and it
  never releases one. On `vz` there is no namespace, only the lease. On `firecracker` the namespace
  holds a tap bridged to the veth, and no address. The jailer puts the vmm in the namespace, the vmm
  opens the tap, and the provider addresses the guest with the lease that the spec carries.
- The host is the policy of record. On Linux that is host netfilter. On `vz` it is the daemon's own
  userspace netstack, which every VM packet crosses. Nothing a sandbox can reach may depend on a
  rule that lives inside the sandbox.

A verb that acts on a sandbox names it by its id, as an argument or in the spec, because a sandbox
outlives the `shard daemon` that created it, and the next daemon finds it by that id alone. `Name`,
`Capabilities`, `CheckResources` and `AdoptStaging` act on no sandbox, so they take no id.

## What the conformance suite proves

`services/provider/conformance` is the suite every substrate imports from its own tests. It proves:

- a sandbox outlives its entrypoint, and only `Stop` ends one;
- `Stop` signals first and kills only when the grace runs out, against an entrypoint that ignores
  SIGTERM and against one that does not;
- `Stop` is idempotent, ends a sandbox that never started, and survives a `Remove`;
- `Remove` force-ends a running sandbox;
- `Status` after `Create`, and `Status` on an id the substrate never held;
- a second `Create` over a used state directory answers no stale exit status;
- `Wait` returns the context error on a cancelled context;
- `Exec` returns the command's own exit code, and two execs share one sandbox;
- `Exec` applies its own env and workdir, and the entrypoint never sees them;
- `Exec` refuses an id the substrate never held, and a sandbox that is stopped, naming both the
  sandbox and its state;
- `Clone` runs the source's entrypoint again under a new id and leaves the source down, and it
  refuses a source that is running, naming both the sandbox and its state;
- `Capabilities` and the verbs agree, and every refusal names the provider and the verb.

Every substrate runs it from its own `*_integration_test.go` under `make itest`. On Sysbox and runc
every snapshot case ends at the refusal, and the suite skips the rest of that verb. So the suite
proves the refuse path there. It proves the snapshot path on gVisor, on `vz` and on Firecracker.
`vzvm_integration_test.go` runs the suite on real VMs on an Apple silicon Mac, and
`firecracker_integration_test.go` runs it on real microVMs on a host with `/dev/kvm`. The suite
takes one fork from one snapshot. The N-fork case, which holds every substrate to a snapshot that
no verb consumes, is SHARD-45.

It proves nothing about the network. Every provider on Linux joins a namespace that the network
service built, and `vz` joins none, so there is nothing to generalize yet.

It does not prove that systemd runs as a sandbox's own init, because no substrate allows it.
`shard-init` is PID 1 on every provider, and systemd refuses the system-manager role when it is not
PID 1. The suite has no systemd case to skip, because the design forbids the case and leaves nothing
to test.
