# The provider contract

One interface, `models.Provider`, and one implementation per substrate. `models/provider.go` is the
code; this page says what the signatures cannot.

## The substrates

`shard daemon --provider <name>` picks one substrate for the whole host, and every sandbox on that
host runs on it. A record names the substrate that made it. Do not switch a host's provider while
records exist: the other substrate has never heard of those sandboxes.

| | gVisor (`gvisor`, the default) | Sysbox (`sysbox`) | Firecracker |
|---|---|---|---|
| Isolation | a user-space kernel, `runsc` | a Linux container, `sysbox-runc`, with a user namespace and virtualised `/proc` and `/sys` | a microVM, needs `/dev/kvm` |
| Syscall cost | high on file-heavy work (`npm install`, `git clone`) | near native | near native |
| Docker or systemd inside | no | yes | yes |
| Tenancy | many tenants on one host | **one tenant per host**, see below | many tenants on one host |
| Status | every verb | every required verb, no snapshot verb | does not exist yet |

The capability table, in CLI names. The first row is the required verbs; the other three are what `Capabilities`
reports and the CLI refuses on:

| Verb | gVisor | Sysbox | Firecracker |
|---|---|---|---|
| `create`, `start`, `stop`, `rm`, `clone`, `exec`, `logs`, `inspect` | yes | yes | planned |
| `pause` | yes | **no** | planned |
| `resume` | yes | **no** | planned |
| `fork` | yes | **no** | planned |

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

## What `Status` means

`Status` asks the substrate and reports what it says now. It never reads the shard record, and the
record never answers for it. The two disagree on purpose:

- a record that says `running` can outlive a `shard` restart, or a sandbox someone killed by hand;
- a sandbox stopped before its entrypoint ran leaves nothing at the substrate, so the record says
  `stopped` and `Status` reports `Exists: false`.

`Status.Alive()` is `Exists && State != stopped`. Only `Stop` and `Pause` take a sandbox out of it. The
entrypoint exiting is not a transition, and `Wait` returning does not end anything.

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

`services/provider/conformance` is the suite both substrates import from their own tests. It proves:

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

Both substrates run it from their own `*_integration_test.go` under `make itest`. On Sysbox every
snapshot case ends at the refusal and the suite skips the rest of that verb, so the suite proves the
refuse path there and the snapshot path only on gVisor. The snapshot-shaped interface questions wait
for Firecracker (SHARD-45).

It does not prove anything about the network: both substrates join a namespace the network service
built, so there is nothing to generalize yet.
