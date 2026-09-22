# The provider contract

One interface, `models.Provider`, and one implementation per substrate. `models/provider.go` is the
code; this page says what the signatures cannot.

## The substrates

`shard daemon --provider <name>` picks one substrate for the whole host, and every sandbox on that
host runs on it. A record names the substrate that made it. Do not switch a host's provider while
records exist: the other substrate has never heard of those sandboxes.

| | gVisor (`gvisor`, the default on Linux) | Sysbox (`sysbox`) | runc (`runc`) | vz (`vz`, the default on macOS) | Firecracker (`firecracker`) |
|---|---|---|---|---|---|
| Isolation | a user-space kernel, `runsc` | a Linux container, `sysbox-runc`, with a user namespace and virtualised `/proc` and `/sys` | **none**: a Linux container, `runc`, on the host kernel with no user namespace | a VM per sandbox on Virtualization.framework, one `shard-vz-shim` each | a microVM, needs `/dev/kvm` |
| Syscall cost | high on file-heavy work (`npm install`, `git clone`) | near native | near native | near native | near native |
| Docker inside | no | yes | no | yes | yes |
| systemd as PID 1 | no | no | no | no | no |
| Tenancy | many tenants on one host | **one tenant per host**, see below | **one tenant per host**, and only code you trust | many tenants on one Mac | many tenants on one host |
| Exit code | host-verified, behind the sentry | **guest-attested**, see below | **guest-attested**: guest root is host root | host-verified, behind the VM | host-verified, behind the VM |
| Status | every verb | every required verb, no snapshot verb | every required verb, no snapshot verb | every verb on Apple silicon with macOS 14+; no snapshot verb on 13 or on Intel | every required verb; no snapshot verb until SHARD-44 |

The capability table, in CLI names. The first row is the required verbs; the other three are what `Capabilities`
reports and the CLI refuses on:

| Verb | gVisor | Sysbox | runc | vz | Firecracker |
|---|---|---|---|---|---|
| `create`, `start`, `stop`, `rm`, `clone`, `exec`, `logs`, `inspect` | yes | yes | yes | yes | yes |
| `pause` | yes | **no** | **no** | Apple silicon on macOS 14+, **no** on 13 or on Intel | **no** until SHARD-44 |
| `resume` | yes | **no** | **no** | Apple silicon on macOS 14+, **no** on 13 or on Intel | **no** until SHARD-44 |
| `fork` | yes | **no** | **no** | Apple silicon on macOS 14+, **no** on 13 or on Intel | **no** until SHARD-44 |

### systemd is not a sandbox's init

**systemd cannot run as a sandbox's PID 1, on any provider.** `shard-init` is PID 1 in every
sandbox and starts the image entrypoint as its child (`cmd/shard-init` uses `ForkExec`), so the
entrypoint is never PID 1. systemd refuses the system-manager role below PID 1: it prints "Explicit
--user argument required to run as user manager." and exits, `systemctl is-system-running` reports
`offline`, and the record stays `running` because a sandbox outlives its entrypoint. This is not a
Sysbox limit. It holds on gVisor and Firecracker too, and it follows from `shard-init` being PID 1 in
every sandbox. Docker inside a sandbox is unaffected and works end to end on Sysbox and on `vz`,
where `dockerd` runs as the entrypoint of a VM (SHARD-247, `docs/provider-vz.md`). To run
systemd, run it inside a Docker container the sandbox starts, not as the sandbox's own init.

### What Sysbox does not do

**Sysbox has no pause, no resume and no fork.** `sysbox-runc` dropped upstream `runc`'s
`checkpoint` and `restore` (nestybox/sysbox#715, open since 2023), so there is no memory image to
take. The provider claims `{Pause: false, Resume: false, Fork: false}` and each verb refuses by name:
`provider sysbox does not support pause on this host`. Nothing is emulated: a `pause` on Sysbox is a
refusal, not a stop, and the sandbox runs on. `clone` still works, because it needs no snapshot.

**Sysbox CE is single-tenant.** Sysbox CE maps every container to the same host uid range,
`0 165536 65536`; exclusive ranges were a Sysbox EE feature, and EE is gone. So two Sysbox sandboxes
are isolated from the host and **not from each other**: a process that escapes one container's
namespaces has the uid of every other sandbox's root. Run one tenant per Sysbox host. Do not place
two customers' sandboxes on one. shard pins that same mapping on the user namespace that owns each
sandbox's netns (`services/provider/sysbox.Userns`), which is why the guest holds `CAP_NET_ADMIN` over
its own interface and nothing else. The upstream fix is an exclusive-range allocator in
`sysbox-mgr`, and until it lands this limit stands.

**Sysbox does not verify the entrypoint's exit code.** `shard-init` reports the exit record on its
fd 0, which the host holds, and clears its dumpable flag so `/proc/1/fd` is out of the guest's reach.
On gVisor that holds: the sentry enforces the capability set the bundle grants, which has no
`CAP_SYS_PTRACE`, so a guest write to `/proc/1/fd/0` fails with `EACCES`. `sysbox-runc` gives guest
root the full capability set whatever the bundle lists, `CAP_SYS_PTRACE` included, so guest root
controls PID 1 and the entrypoint: it can write a forged record, drive the exit value or pick the
signal, and a background write after the real exit makes `inspect` report the forged code. No channel
on Sysbox is host-readable and guest-unwritable, so there is no mechanism fix: the exit code of a
Sysbox sandbox is what its root attests, which on a single-tenant host is your own code. Firecracker
will verify it behind the VM boundary the way gVisor does behind the sentry.

**Sysbox runs where `sysbox-runc` runs.** It needs the Sysbox package installed on the host, root,
and a kernel Sysbox supports. There is no fallback to gVisor: a host without `sysbox-runc` gets a
daemon that answers the reads and refuses every sandbox verb, the same as a gVisor host without
`runsc`.

### What runc does not do

**runc isolates nothing.** The guest shares the host kernel with only namespaces and cgroups between
them, and root in the guest is root on the host: use it for code you trust or a box you can lose. It
is never picked by default; `--provider runc` is the only way onto it. It has no pause, no resume and
no fork, because shard drives no checkpoint on it, and each verb refuses by name: `provider runc does
not support pause on this host`. It runs where the `runc` package is installed, root, and there is no
fallback to gVisor.

## Required verbs against optional verbs

Fourteen verbs are required. Every substrate must do all of them, and none of them has a capability
flag: `CheckResources`, `Create`, `Start`, `Stop`, `Remove`, `Clone`, `Exec`, `Signal`, `Wait`,
`ExitStatus`, `Status`, `Restarts`, `LogPath`, and `Capabilities` itself.

`CheckResources` answers whether the substrate can run under a bound before the orchestrator writes
a record, so a refusal leaves nothing in `ls`. Only vz refuses anything: a VM's memory is real
memory, so `--memory 0` and a bound under 128 MiB are refused by name. The Linux substrates take
every bound. `Create` checks its spec again, so a clone or a fork is held to the same rule.

`Clone` is required because it needs nothing a substrate may lack: it copies the writable layer
another sandbox kept and runs that sandbox's entrypoint again, from the beginning, under the new id
and the new network. It refuses a source that is alive or still mounted, and it reads nothing of the
source but its status and its state directory. Firecracker reflinks `overlay.raw` where gVisor copies
an overlay layer: the clone shares the source's blocks, and a root whose filesystem cannot (ext4,
tmpfs) refuses the clone by name rather than copy every byte. XFS and Btrfs can.

Three verbs are optional: `Pause`, `Resume`, `Fork`. `Capabilities` reports one boolean per optional
verb, and it is the only place a substrate is allowed to be unequal to another.
`fork` takes a paused source on every substrate: the snapshot is what the pause wrote, a resume runs
on past it and keeps its name, so a running or stopped source is refused even when its record still
names one.

### What vz does and does not do

`vz` is the substrate `shard daemon` picks on a Mac when `--provider` is empty, and it is refused by
name anywhere else. Each sandbox is one Virtualization.framework VM booting shard's own arm64 kernel
(`services/kernel` fetches the release once under the root, `SHARD_KERNEL` and `SHARD_KERNEL_SHA256`
override it) over an APFS clone of the image's ext4 disk, held by one `shard-vz-shim` the daemon
signs under `<root>/vz` from the copy `make build-darwin` embeds, beside the static linux `shard-init`
that build embeds for this Mac's arch and the daemon installs under `<root>/vz` as the initrd's
`/init`; a `go build` alone has neither and the first sandbox says so. `SHARD_INIT_PATH` names a
guest `shard-init` of your own instead. `--memory` is required, `0` is refused by name, and it is a hard cap. There is no bridge: the daemon
leases each guest an address from the pool, terminates its frames in a userspace stack that answers
for the gateway alone, and serves the proxy and the resolver on that stack. The stack's own NAT
table sends a guest's port 80 and 443 to the proxy wherever the guest dialed them, as the host
chains do on Linux, and every other TCP or UDP flow is judged by the same compiled chains the host
ruleset is built from, so a policy means the same on both hosts (SHARD-246); a refused flow is
dropped in the stack and written to the sandbox's egress log, which `docs/provider-vz.md` covers.
`pause`, `resume` and `fork` are one VZ save and a restore, which macOS 14 added on Apple silicon: on 13, and on an Intel Mac, all three refuse by name.
The three resource bounds below hold on the Linux substrates; `vz` and `firecracker` have no host
cgroup, and each section says what the VM does instead.

### What Firecracker does and does not do

`firecracker` is `--provider firecracker` on a Linux host with `/dev/kvm` and the `firecracker`
binary on PATH, one `firecracker` process per sandbox, driven over its API socket in the sandbox's
state directory. Each one boots shard's own amd64 kernel (`services/kernel` fetches the release
once under the root, `SHARD_KERNEL` and `SHARD_KERNEL_SHA256` override it) from what the section
below describes: the image's EROFS file read-only, the sandbox's `overlay.raw`, and an initrd of
the static `shard-init` at `SHARD_INIT_PATH`, which the daemon writes once under
`<root>/firecracker`. The host needs `erofs-utils` for the pull. The host speaks to the guest over
vsock alone, through the socket firecracker proxies it on, so `exec`, `logs` and the exit come the
way they do on `vz`. The vmm has no stop of its own: a stop tells `shard-init` to end the
entrypoint and reboot, which is the one guest exit firecracker ends its process on (a power off
leaves it running), and the grace runs out into a kill of the process. A guest the host can no
longer reach over vsock is still a running VM: `inspect` says so, and `stop` kills it without a
grace it could not hear. A daemon restart adopts a running vmm by its socket. `--memory` is required, `0` is refused by name, and 128 MiB is
the least a guest boots with. The guest's network is a tap on the same bridge the veth substrates
use, named `shardv<n>` like a veth and a port under the same host rules: the anti-spoof pair, the
IPv6 drop and the egress chain key on that name, the proxy redirect on the leased address and the
private floor on the bridge, so a policy reads and logs the same on every substrate. The vmm opens the tap as the
guest's `eth0` with a MAC derived from the lease, and `shard-init` takes the address, the gateway
and the resolver over vsock once the guest is up, before the entrypoint runs. The next start after
a stop leases the same address and builds the tap again for the new vmm; `rm` releases both.
`pause`, `resume` and `fork` refuse by name until SHARD-44 lands the snapshot.

`scripts/e2e-fc.sh`, behind `make e2e-firecracker`, drives the whole lifecycle on it: the daemon
over a root it turns into an XFS image, `create` with `--memory`, `logs`, `exec`, an entrypoint
that exits, the policy and the proxy on the tap, a daemon restart that adopts the vmm, a vmm lost
while the daemon was down, the three snapshot refusals, `stop`, two clones by reflink, `start`,
`rm`, and a host with no tap, no vmm, no image and no fstab line left. It runs on demand only.
It needs `/dev/kvm`, which no CI runner and no cloud devbox has, so CI, `make check`, `make e2e`
and `make devbox-e2e` never call it: rent a bare-metal KVM box, run `sudo make e2e-firecracker`
there with `erofs-utils`, `xfsprogs`, `firecracker` and Go on it, and destroy the box. `SHARD_KERNEL`
and `SHARD_KERNEL_SHA256` point it at a kernel on the box; unset, the daemon fetches the release.

The guest reaches the resolver and the proxy on the bridge address, so a host firewall that drops
`INPUT` eats those packets after shard's own table accepted them. A rented box with `ufw` on is the
common case. Run `iptables -I INPUT -i shard0 -j ACCEPT` there, or `ufw allow in on shard0`, before
the suite; the host check fails by name when the policy is `DROP` and no such rule exists.

## Refuse, never downgrade

A provider that cannot do an optional verb returns `models.Unsupported(provider, verb)`. That error
names both, and it unwraps to `models.ErrUnsupported`. The CLI asks `Capabilities` first and refuses
before it holds or claims anything, so the provider's own refusal is the last line of defence. It never falls back to a weaker mechanism, and
it never returns a nil error having done something else.

The sentinel is reserved for that one meaning. A missing kernel feature on a developer Mac is not a
refused verb, so `bundle`'s overlay stub and `netns.ErrNotLinux` are plain errors.

`Capabilities` is computed once, in the constructor, and the method is a cheap getter. A probe that
needed a context and an error would be a fourth thing to get wrong.

## What a memory bound means

`--memory` bounds a sandbox the same way on every substrate: past the bound the whole sandbox dies,
not one process inside it, and the daemon restarts it when the record set `restart_on_oom`. gVisor
sets `memory.oom.group=1` and `memory.swap.max=0` on the host cgroup; Sysbox and runc set the same
pair. `sysbox-runc` and `runc` apply `memory.max` from the bundle but neither knob, so without them
the OOM killer took one guest process, the sandbox lived, and `oom_restarts` stayed at zero.
On `vz` the bound is the VM's memory, and `shard-init` puts the same pair on a cgroup inside the
guest: `memory.max` is the VM's memory less 32 MB of headroom for the kernel and `shard-init`
itself, with `memory.oom.group=1` and `memory.swap.max=0`. `shard-init` moves into that cgroup and
unshares a cgroup namespace rooted there, so everything a guest starts, a Docker daemon and its
containers included, lands under the bound; `shard-init` alone is exempt, through
`oom_score_adj=-1000`, and each child it forks runs `shard-init -expose` first, which gives the
exemption up before the workload can fork. When the killer takes the group, `shard-init` reads
`memory.events.local` and reports the kill over vsock instead of an exit. The guest then holds
that state and does not power off on its own: the host writes the `oom` marker first and only then
sends the stop, so a daemon that dies between the report and the marker finds the kill again in
the state the next connection replays, and marks it then. Once the marker is down `Status` says
`OOMKilled`, no exit record lands, and the daemon restarts the sandbox on `restart_on_oom` exactly
as it does on Linux. A kill that finds no host attached, during a reconnect after a sleep, waits
the same way for that replay. The bound needs room under the headroom, so `vz` refuses a
`--memory` below 128 MiB by name.

## What a cpu bound means

`--cpus` bounds a sandbox to a share of the host CPUs, the same way on every substrate. `--cpus 0`,
the default, sets no bound: `cpu.max` stays `max` and the sandbox runs on every host CPU. A positive
`N` caps it at `N` CPUs of run time, as a `cpu.max` quota of `N * 100000` over a `100000` period.
`shard create` refuses a negative value with an error, because a bound below zero is not a spelling
of unbounded, and a fraction such as `0.5` with an error that names it, because a VM gets whole CPUs
and a rounded bound is not the one asked for. On `vz` the count is the VM's virtual CPUs, not a quota:
`--cpus 0` gives it one per host CPU, held inside the framework's ceiling, and a positive `N` gives it `N`,
refused outside the host's range.

## What the pids bound means

Every sandbox cgroup holds at most `4096` processes. Unlike memory and cpu, the bound is never off
and not adjustable: there is no flag and no field, and `pids.max` is written at create and again on
every launch, so a sandbox created before the bound existed is capped when it runs again. `4096` runs
systemd, Docker and nested containers with headroom, yet stops a fork bomb far below the host PID
count. The bound is fixed because on Sysbox a guest process is a host process, so an unbounded fork
bomb in one sandbox takes the host down; on gVisor the bomb stays in the sentry and hits `memory.max`
first, but the same bound applies. On `vz` there is no bound: a guest process is a process inside
the VM, so a fork bomb stays there and hits the VM's memory, and the host is untouched.

## What a disk bound means

`--disk` bounds everything a sandbox can write, the same way on every substrate. The writable layer
over the image, `/tmp` and the supervisor's files under `/.shard` all sit on one ext4 image per sandbox,
`disk.img` in its state directory, which the daemon loop-mounts at `disk/` before the overlay stacks on
it. The bound is never off: `--disk 0`, the default, is `10240` MiB, and a positive `N` overrides
it. The image is sparse, so an unwritten sandbox costs the host nothing, and it is truncated
to the bound, so host usage stops there whatever the guest does. Inside the guest a write past the bound
fails with `ENOSPC`, the sandbox lives on, and `df` shows the bound less what ext4 keeps for itself.

A stop detaches the disk and a start mounts it again, so the layer survives the way it did before. On
Sysbox the disk stays mounted while `sysbox-runc` holds the stopped sandbox: `sysbox-mgr` chowns the
upper layer back when the container is deleted, at the next start or at `rm`, and it must find it. Fork
and clone copy the layers into a disk of their own, bounded the way the source was; config.json carries
the bound for that. The record carries the resolved bound, so `inspect` shows the value the image
enforces, not a bare `0`. `shard create` refuses a negative value. The host needs `mkfs.ext4`, which
`e2fsprogs` ships. On Sysbox the directories `sysbox-runc` backs from the host, `/var/lib/docker` among
them, sit outside the image and so outside the bound.

## What a microVM boots from

A microVM provider, Firecracker first, boots no OCI bundle and no ext4 root disk: it boots a
read-only EROFS image of the pulled image, an `overlay.raw` of its own, and an initrd. The image
service builds the EROFS image once per digest under `images/erofs/<digest>.erofs`, from the same
unpacked tree the bundle path uses, with `mkfs.erofs` at 4 KiB blocks so an image built on a
16 KiB-page host mounts in a 4 KiB-page guest; the host needs `erofs-utils`. `overlay.raw` is one
empty ext4 image per sandbox, written in Go and grown to the disk bound, so the bound above holds
the same way: every write of the sandbox lands on it and stops there. The initrd is `shard-init`
alone, as `/init` of a newc archive, the same file the vz provider boots. The kernel command line
hands it `-base` and `-overlay`, the two block devices, and `-console` for where its stderr goes:
`shard-init` mounts the EROFS image read-only, the ext4 disk over it, lays `upper` and `work` on
that disk, mounts the overlay as the root, moves the kernel filesystems across and pivots onto it,
exactly as the one-disk `-root` boot does. It stays PID 1, and the host sends the entrypoint over
vsock as before. `vz` keeps its ext4 root disk: an APFS clone is its overlay. The guest kernel must
carry `CONFIG_EROFS_FS` and `CONFIG_OVERLAY_FS`; neither shipped kernel config sets the first yet,
which SHARD-265 adds; on a host whose kernel lacks it the boot fails at the base mount, and the console log says so.

## What `Status` means

`Status` asks the substrate and reports what it says now. It never reads the shard record, and the
record never answers for it. The two disagree on purpose:

- a record that says `running` can outlive a `shard` restart, or a sandbox someone killed by hand;
- a sandbox stopped before its entrypoint ran leaves nothing at the substrate, so the record says
  `stopped` and `Status` reports `Exists: false`.

`Status.Alive()` is `Exists && State != stopped`. Only `Stop` and `Pause` take a sandbox out of it. The
entrypoint exiting is not a transition, and `Wait` returning does not end anything. Under a restart
policy `Wait` returns the first exit of the run, not the settled one: the supervisor rewrites the exit
file per exit and clears nothing, so only a stopped sandbox answers with its last exit. No verb waits
on the entrypoint; `stop` is the one caller, and it reads after the sandbox is down.

## What `Exec` means

`Exec` is a second process in a sandbox that already runs. It is never the entrypoint: the supervisor
does not see it, its exit ends nothing, and a signal that ends it never reaches the sandbox. Only
`Stop` ends a sandbox.

It reports the command's own exit code, which is the opposite of `Create` and `Start`, and the reason
the verb is useful. An error means the exec never ran; an exit code means it did.

`ExecSpec.User` empty is the user the entrypoint runs as, the way `docker exec` inherits it. The
supervisor's own process user is root, so the record of it is the `-user uid:gid` in the supervisor's
argv, which the provider reads back from the sandbox.

`ExecSpec` carries `*os.File`, not `io.Reader`. A TTY is one pty replica the caller allocates on the
host, and a pipe cannot be one.

## Who owns what

- The **provider** owns the whole layout inside `StateDir`. Nothing outside it may name a file there.
- The **repository** owns the record and the directory itself. It removes the directory only after
  `Remove` has dropped every mount inside it.
- The **network service** owns the namespace, the address and the host interface. `NetworkSpec` is
  allocated before `Create`, so a provider joins a namespace it did not build and never releases one.
  On `firecracker` the host interface is a tap and there is no namespace: the vmm opens the tap, and
  the provider addresses the guest with the lease the spec carries.
- **The host is the policy of record.** On Linux that is host netfilter; on `vz` it is the daemon's own
  userspace netstack, which every VM packet crosses. Nothing a sandbox can reach may depend on a rule that
  lives inside the sandbox.

Every verb takes an id, because `shard` runs no daemon that could remember anything from `Create`.

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
every snapshot case ends at the refusal and the suite skips the rest of that verb, so the suite proves
the refuse path there and the snapshot path on gVisor and on `vz`, whose `vzvm_integration_test.go` runs
the suite on real VMs on an Apple silicon Mac, and Firecracker, whose `firecracker_integration_test.go` runs it on
real microVMs on a host with `/dev/kvm`. The snapshot-shaped interface questions wait for the Firecracker
snapshot (SHARD-44).

It does not prove anything about the network: every substrate joins a namespace the network service
built, so there is nothing to generalize yet.

It does not prove systemd runs as a sandbox's own init, because no substrate allows it: `shard-init`
is PID 1 on every provider and systemd refuses the system-manager role below PID 1. The suite has no
systemd case to skip; the design forbids the case, so there is nothing to test.
