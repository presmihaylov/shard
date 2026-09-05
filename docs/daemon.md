# The daemon

`shard daemon` is the resident process that owns the state. The CLI is becoming a thin client of
it, one verb at a time: a verb that speaks the socket never falls back to the files, and fails fast
without a daemon, with one line:

```
shard: cannot connect to shard daemon at /var/lib/shard/shard.sock: is it running? systemctl status shard
```

So far `version`, `ls`, `inspect`, `create`, `start`, `stop`, `rm`, `exec`, `logs`, `pause`,
`resume`, `fork` and `clone` speak the socket. Every other verb still works on the on-disk stores
directly, under the same lockfiles the daemon takes, whether the daemon is up or not. No verb starts the daemon: a resident root process is installed on purpose, through the systemd unit in
`packaging/systemd/shard.service`:

```
cp packaging/systemd/shard.service /etc/systemd/system/
systemctl enable --now shard
```

## What the daemon owns

- **The API socket**: the REST surface under `${root}/shard.sock`, described below.
- **Proxy supervision**: start the egress proxy, health-check it, restart it after a crash, and
  re-front every sandbox that depends on it.
- **Egress log rotation**: the per-sandbox `egress.jsonl` files grow without it.
- **The OOM watchdog**: a host OOM kill takes a sandbox's sentry, and only a resident process can
  bring it back.

- **The sandbox lifecycle**: `create`, `start`, `stop`, `rm`, `exec`, `logs`, `pause`, `resume`,
  `fork` and `clone` run inside the daemon, in `services/sandbox`. The image pull of a create happens there too, and the client waits for it with
  no deadline; the pull's progress is not streamed back to the client yet. The daemon serializes the
  verbs on one sandbox with an in-process mutex per id, so a stop and an rm on the same sandbox run
  one after the other and two on different sandboxes run side by side.

The daemon holds the guest side of an exec: its pipes, and the pseudo terminal of a `-t` exec. The
CLI keeps the local terminal alone, which is raw mode and the `SIGWINCH` it forwards.

A sandbox outlives the daemon. runsc runs in its own session, so a daemon that stops or restarts
leaves every sandbox up, and the next daemon lists and serves them from the same root. The unit
sets `KillMode=process` for the same reason: systemd ends the daemon alone, never its sandboxes.

## The API socket

The daemon listens on `${root}/shard.sock`, so `/var/lib/shard/shard.sock` by default. It never
listens on TCP. The socket is `0660 root:shard` when the host has a `shard` group, else `0600 root`,
and the daemon logs which at startup:

```
api listening on /var/lib/shard/shard.sock, mode 0660, group shard
```

A unix socket needs write permission to connect, so the unit sets `UMask=0077`: the socket is never
world-writable between the listen and the chmod. A stale socket file from a daemon that was killed
is removed before the listen; the singleton lock is already held by then, so a live daemon's socket
is never removed. The socket goes away with the daemon.

The routes:

```
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/version
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/sandboxes
curl --unix-socket /var/lib/shard/shard.sock 'http://localhost/v0/sandboxes?all=true'
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/sandboxes/<id or name>
curl --unix-socket /var/lib/shard/shard.sock -X POST -d '{"image":"alpine:3.20","command":["sleep","600"]}' http://localhost/v0/sandboxes
curl --unix-socket /var/lib/shard/shard.sock -X POST http://localhost/v0/sandboxes/<id or name>/start
curl --unix-socket /var/lib/shard/shard.sock -X POST -d '{"grace":10}' http://localhost/v0/sandboxes/<id or name>/stop
curl --unix-socket /var/lib/shard/shard.sock -X DELETE 'http://localhost/v0/sandboxes/<id or name>?force=true&grace=10'
curl --unix-socket /var/lib/shard/shard.sock -X POST http://localhost/v0/sandboxes/<id or name>/pause
curl --unix-socket /var/lib/shard/shard.sock -X POST http://localhost/v0/sandboxes/<id or name>/resume
curl --unix-socket /var/lib/shard/shard.sock -X POST -d '{"name":"web-2"}' http://localhost/v0/sandboxes/<id or name>/fork
curl --unix-socket /var/lib/shard/shard.sock -N 'http://localhost/v0/sandboxes/<id or name>/logs?follow=true'
```

- `GET /v0/version` answers `{"version": "..."}`, which `shard version` prints as its `daemon` line
  under the `client` line of the binary that asked. `shard --version` prints the `client` line alone,
  touches no socket, and never fails, as `docker --version` does.
- `GET /v0/sandboxes` answers `{"sandboxes": [...]}` as `shard ls` lists them: stopped sandboxes
  hidden unless `all=true`. When some records are unreadable it still answers 200 with the readable
  sandboxes and a `warnings` array, one string per unreadable record; `ls` prints the table and then
  the warnings on stderr, and exits non-zero. It answers 500 only when the list itself failed.
- `GET /v0/sandboxes/{id}` takes an id or a name and answers the record, with an `egress` object
  beside it when the record names a policy: what the host enforces, as `shard inspect` prints it.
  404 when nothing has it, which `inspect` prints as `no sandbox <ref>`; 400 when the reference
  does not validate; 500 for anything else: an unreadable record, a name link at something that is
  not a sandbox, or a policy that cannot be compiled.

- `POST /v0/sandboxes` takes `{"image", "name", "command", "env", "workdir", "user", "secrets",
  "policy", "resources": {"memory_mib", "vcpus"}}`, pulls the image, builds and starts the sandbox,
  and answers 201 with the record. 400 when the body does not decode or a field does not validate,
  or when it names a secret or a policy the host does not hold; 500 when a claim fails, and the
  daemon has then given back everything it built.
- `POST /v0/sandboxes/{id}/start` takes no body and answers 200 with the record of the sandbox it
  ran again. 404 when nothing has the reference; 409 when the sandbox is not stopped.
- `POST /v0/sandboxes/{id}/stop` takes `{"grace": <seconds>}`, the default being 10, waits the grace
  out and answers 200 with the stopped record. 400 for a negative grace; 404; 409 when the sandbox
  is not running.
- `DELETE /v0/sandboxes/{id}` answers 204 with no body. 404; 409 when the sandbox is still up,
  unless `?force=true`, which stops it first with `grace=<seconds>` from the query.
- `POST /v0/sandboxes/{id}/pause` takes no body and answers 200 with the paused record. 404; 409
  when the sandbox is not running, or when the provider does not claim the verb.
- `POST /v0/sandboxes/{id}/resume` takes no body and answers 200 with the running record. 404; 409
  when the sandbox is not paused, when its record names no snapshot, or for an unclaimed verb.
- `POST /v0/sandboxes/{id}/fork` takes `{"name"}` and answers 201 with the new record, run from the
  source's snapshot. 400 for a body that does not decode or a name that does not validate; 404; 409
  when the source has no snapshot, or for an unclaimed verb.
- `POST /v0/sandboxes/{id}/clone` takes `{"name"}` and answers 201 with the new record, run from a
  copy of the source's files. 400 as fork; 404; 409 when the source is still up.

- `POST /v0/sandboxes/{id}/exec` takes `{"command", "env", "workdir", "user", "stdin", "tty",
  "size": {"rows", "cols"}}` with `Connection: Upgrade` and `Upgrade: tcp`, and answers 101 with
  `X-Shard-Exec-Id: <exec id>`. Everything after the 101 is frames, both ways. 400 for a body that
  does not decode or a request that names no command; 404; 409 when no command can run in the
  sandbox. Every refusal comes before the 101, so it is a status and a JSON body like any other.
- `POST /v0/sandboxes/{id}/exec/{exec-id}/resize` takes `{"rows", "cols"}` and answers 204. 404 when
  that exec has ended or belongs to another sandbox. Only a `tty` exec has a terminal to resize.
- `GET /v0/sandboxes/{id}/logs` answers 200 `text/plain; charset=utf-8` with everything the
  entrypoint wrote. With `?follow=true` it streams and flushes every write, and ends when the
  sandbox stops or the client goes away. 404; 400 for a `follow` that is not a boolean.

A frame is an 8-byte header and its payload: one byte of stream, three zero bytes, then the payload
length as a 4-byte big-endian number. A payload is at most 1 MiB, and a longer write goes as several
frames. The client sends stream 0 (stdin) and 4 (stdin closed); the daemon sends 1 (stdout), 2
(stderr), 5 (a failure of the daemon's own) and 3 (exit), whose payload is the exit code in decimal.
The exit frame ends the session, with a terminal or without one. A `tty` exec carries the guest's
terminal on stream 1 alone, because a terminal has no second stream to keep apart.

A 409 body is the refusal as the CLI prints it: `sandbox <id> is <state>: <fix>`. A verb the
provider does not claim is a 409 too: `provider <name> does not support <verb> on this host`.

Every error body is `{"error": "<message>"}`.

The typed side of these routes is `services/client`: `Version`, `ListSandboxes`, `GetSandbox`,
`CreateSandbox`, `StartSandbox`, `StopSandbox`, `RemoveSandbox`, `PauseSandbox`, `ResumeSandbox`,
`ForkSandbox`, `CloneSandbox`, `Exec`, `ResizeExec` and `Logs`,
hand-written over the socket. `Exec` dials the socket, writes the request itself and reads the 101,
because `net/http` gives no connection back; `Logs` holds its stream open for as long as the follow
lasts. Neither takes the 30 s deadline the answered-in-full calls take.
The CLI verbs call it and nothing else. Each call that answers in full gets 30 s, per request and
not on the `http.Client`; a daemon that accepts and never answers fails as `GET <route> on <socket>:
no answer within 30s`. `CreateSandbox` sets no deadline, because the pull inside it has none the
client could know, and neither do the four snapshot verbs, because a checkpoint takes as long as
the memory and the disk it writes; `StopSandbox` and `RemoveSandbox` add the grace to theirs.

## One daemon per root, and liveness

The daemon takes an exclusive flock on `daemon.lock` under the root and refuses to start while
another holds it. An `Alive` probe holds that lock for a moment itself, so a starting daemon
outwaits a contended lock briefly before it calls the holder a daemon. The same lock is the
liveness signal: it dies with the process, so there is no stale pid file. That signal is advisory.
`Alive` true means a daemon holds the lock right now, and the caller must still tolerate it dying a
moment later; anything that needs the daemon checks outcomes, not liveness.

## Supervision

Each piece of background work is a task. A task that returns an error is restarted with exponential
backoff and jitter, capped at a minute; a run that stays up long enough earns a fresh backoff, so a
crash loop backs off and a one-off crash restarts fast. A panic in a task is contained and counts as
a failure. A task that returns nil is done and stays done until the daemon restarts. The `api` task
returns an error when its listener dies, so it is restarted, and nil only when the daemon is ending.
