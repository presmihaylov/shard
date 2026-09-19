# The provider contract

One interface, `models.Provider`, and one implementation per substrate. `models/provider.go` is the
code; this page says what the signatures cannot.

## The substrates

`shard daemon --provider <name>` picks one substrate for the whole host, and every sandbox on that
host runs on it. A record names the substrate that made it. Do not switch a host's provider while
records exist: the other substrate has never heard of those sandboxes.

| | gVisor (`gvisor`, the default) | Sysbox (`sysbox`) | runc (`runc`) | Firecracker |
|---|---|---|---|---|
| Isolation | a user-space kernel, `runsc` | a Linux container, `sysbox-runc`, with a user namespace and virtualised `/proc` and `/sys` | **none**: a Linux container, `runc`, on the host kernel with no user namespace | a microVM, needs `/dev/kvm` |
| Syscall cost | high on file-heavy work (`npm install`, `git clone`) | near native | near native | near native |
| Docker inside | no | yes | no | yes |
| systemd as PID 1 | no | no | no | no |
| Tenancy | many tenants on one host | **one tenant per host**, see below | **one tenant per host**, and only code you trust | many tenants on one host |
| Status | every verb | every required verb, no snapshot verb | every required verb, no snapshot verb | does not exist yet |

The capability table, in CLI names. The first row is the required verbs; the other three are what `Capabilities`
reports and the CLI refuses on:

| Verb | gVisor | Sysbox | runc | Firecracker |
|---|---|---|---|---|
| `create`, `start`, `stop`, `rm`, `clone`, `exec`, `logs`, `inspect` | yes | yes | yes | planned |
| `pause` | yes | **no** | **no** | planned |
| `resume` | yes | **no** | **no** | planned |
| `fork` | yes | **no** | **no** | planned |

### systemd is not a sandbox's init

**systemd cannot run as a sandbox's PID 1, on any provider.** `shard-init` is PID 1 in every
sandbox and starts the image entrypoint as its child (`cmd/shard-init` uses `ForkExec`), so the
entrypoint is never PID 1. systemd refuses the system-manager role below PID 1: it prints "Explicit
--user argument required to run as user manager." and exits, `systemctl is-system-running` reports
`offline`, and the record stays `running` because a sandbox outlives its entrypoint. This is not a
Sysbox limit. It holds on gVisor and Firecracker too, and it follows from `shard-init` being PID 1 in
every sandbox. Docker inside a sandbox is unaffected and works end to end on Sysbox. To run
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

Ten verbs are required. Every substrate must do all of them, and none of them has a capability
flag: `Create`, `Start`, `Stop`, `Remove`, `Clone`, `Exec`, `Wait`, `Status`, `LogPath`, and
`Capabilities` itself.

`Clone` is required because it needs nothing a substrate may lack: it copies the writable layer
another sandbox kept and runs that sandbox's entrypoint again, from the beginning, under the new id
and the new network. It refuses a source that is alive or still mounted, and it reads nothing of the
source but its status and its state directory. Firecracker will copy a disk where gVisor copies an overlay layer.

Three verbs are optional: `Pause`, `Resume`, `Fork`. `Capabilities` reports one boolean per optional
verb, and it is the only place a substrate is allowed to be unequal to another.

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

## What a cpu bound means

`--cpus` bounds a sandbox to a share of the host CPUs, the same way on every substrate. `--cpus 0`,
the default, sets no bound: `cpu.max` stays `max` and the sandbox runs on every host CPU. A positive
`N` caps it at `N` CPUs of run time, as a `cpu.max` quota of `N * 100000` over a `100000` period.
`shard create` refuses a negative value with an error, because a bound below zero is not a spelling
of unbounded.

## What the pids bound means

Every sandbox cgroup holds at most `4096` processes. Unlike memory and cpu, the bound is never off
and not adjustable: there is no flag and no field, and `pids.max` is written at create and again on
every launch, so a sandbox created before the bound existed is capped when it runs again. `4096` runs
systemd, Docker and nested containers with headroom, yet stops a fork bomb far below the host PID
count. The bound is fixed because on Sysbox a guest process is a host process, so an unbounded fork
bomb in one sandbox takes the host down; on gVisor the bomb stays in the sentry and hits `memory.max`
first, but the same bound applies.

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
- **Host netfilter is the policy of record.** Nothing a sandbox can reach may depend on a rule that
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
the refuse path there and the snapshot path only on gVisor. The snapshot-shaped interface questions wait
for Firecracker (SHARD-45).

It does not prove anything about the network: every substrate joins a namespace the network service
built, so there is nothing to generalize yet.

It does not prove systemd runs as a sandbox's own init, because no substrate allows it: `shard-init`
is PID 1 on every provider and systemd refuses the system-manager role below PID 1. The suite has no
systemd case to skip; the design forbids the case, so there is nothing to test.
