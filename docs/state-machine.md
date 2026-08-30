# The sandbox state machine

Four states, seven legal moves. `models/state.go` is the code; this page is the picture.

```mermaid
stateDiagram-v2
    [*] --> created: shard create
    created --> running: start
    created --> stopped: stop before start
    running --> paused: pause (snapshot to disk, memory freed)
    running --> stopped: stop, and nothing else
    paused --> running: resume (the snapshot survives)
    paused --> stopped: stop
    stopped --> running: start (over the preserved writable layer)
    stopped --> [*]: rm
    paused --> [*]: rm
    created --> [*]: rm
```

| From | To | Verb | Reachable today |
|---|---|---|---|
| `created` | `running` | `start` | yes |
| `created` | `stopped` | `stop` before the entrypoint runs | yes |
| `running` | `paused` | `pause` | yes: gVisor |
| `running` | `stopped` | `stop` | yes |
| `paused` | `running` | `resume` | yes: gVisor |
| `paused` | `stopped` | `stop` | yes |
| `stopped` | `running` | `start` | yes |

## What the picture does not say

**A sandbox outlives its entrypoint, so the entrypoint exiting is not a transition.** `running` means
the sandbox is up, not that a workload executes in it. When the entrypoint finishes the sandbox stays
`running` and you can still `exec`, `pause` or `fork` it. This is what E2B, Modal, Vercel and Daytona
all do. There is no fifth state for it: the record keeps the last exit status instead, so `shard ps`
can print `running (exited 0)`. **`stop` is the only thing that ends a sandbox.**

**`stopped` is not terminal, and it is not a provider verb either.** Every sandbox has its own
writable layer over the shared read-only image, so `shard start` re-runs the entrypoint and finds the
files the last run wrote. Memory, pids and sockets are gone; files are not. The substrate cannot do
that move: `Provider.Start` refuses a stopped sandbox, so `shard start` (SHARD-24) is `Remove` plus a
second `Create` over the preserved writable layer.

**`created --> stopped` leaves nothing at the substrate.** Stopping a sandbox whose entrypoint never
ran is a delete there, because a runtime refuses to signal a container that never started. The record
says `stopped` while `Provider.Status` reports `Exists: false`. That is the intended answer: the
record is what survives, and `Status` is only ever what the substrate says now. A paused sandbox is
the second case: the record says `paused` and `Status` reports `Exists: false`, because the pause
deleted it from the substrate and only the snapshot holds it. A `pause` also kills any `exec` in flight.

**There is no `checkpointed` state.** `pause` writes the snapshot to disk and frees the memory, so a
paused sandbox holds no RAM. There is no in-memory pause to distinguish it from. `checkpoint` and
`restore` survive only as the `runsc` command names, never in the CLI, the API or the state records.

**`rm` is not a state.** It removes the record and everything under it. `Provider.Remove` force-ends
a running sandbox rather than refusing one, because nothing else drops the rootfs mount.

**`fork` is not a transition.** It creates a second sandbox in `running` and leaves the source in
whatever state it was in. A snapshot is immutable and a resume does not consume it, so one `pause`
plus `fork --count N` is the warm pool primitive.

**`clone` is not a transition either.** It creates a second sandbox in `running` over a copy of the
files a `stopped` or `paused` source kept, and runs the entrypoint from the beginning: a `start`
under a new id. It takes no memory and reads no snapshot, so it is a required verb on every provider,
and it refuses a `running` source rather than copy a layer that is still being written.

**A legal move is not always a possible move.** `State.CanTransitionTo` answers only whether the
move is in the machine. Whether it can happen now is the orchestrator's question, and whether this
substrate can do it at all is the provider's: `pause`, `resume` and `fork` are optional verbs, and a
provider that lacks one reports `false` from `Capabilities` and returns `ErrUnsupported`.
