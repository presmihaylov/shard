# The provider contract

One interface, `models.Provider`, and one implementation per substrate. `models/provider.go` is the
code; this page says what the signatures cannot.

## Required verbs against optional verbs

Nine verbs are required. Every substrate must do all of them, and none of them has a capability
flag: `Create`, `Start`, `Stop`, `Remove`, `Exec`, `Wait`, `Status`, `LogPath`, and `Capabilities`
itself.

Three verbs are optional: `Pause`, `Resume`, `Fork`. `Capabilities` reports one boolean per optional
verb, and it is the only place a substrate is allowed to be unequal to another.

## Refuse, never downgrade

A provider that cannot do an optional verb returns `models.Unsupported(provider, verb)`. That error
names both, and it unwraps to `models.ErrUnsupported`. It never falls back to a weaker mechanism, and
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
- `Capabilities` and the verbs agree, and every refusal names the provider and the verb.

It does not prove anything about the network: there is one substrate today, so there is nothing to
generalize. SHARD-45 owns that when Firecracker lands.
