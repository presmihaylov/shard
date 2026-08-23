# The provider contract

One interface, `models.Provider`, and one implementation per substrate. `models/provider.go` is the
code; this page says what the signatures cannot.

## Required verbs against optional verbs

Eight verbs are required. Every substrate must do all of them, and none of them has a capability
flag: `Create`, `Start`, `Stop`, `Remove`, `Wait`, `Status`, `LogPath`, and `Capabilities` itself.

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

`Status.Alive()` is `Exists && State != stopped`. Only `Stop` takes a sandbox out of it. The
entrypoint exiting is not a transition, and `Wait` returning does not end anything.

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
- `Capabilities` and the verbs agree, and every refusal names the provider and the verb.

It does not prove anything about the network: there is one substrate today, so there is nothing to
generalize. SHARD-45 owns that when Firecracker lands.
