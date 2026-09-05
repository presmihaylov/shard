# The daemon

`shard daemon` is the resident process that owns the state. The CLI is a thin client of it: every
verb goes over the socket, none of them reads or writes the state itself, and each fails fast
without a daemon, with one line:

```
shard: cannot connect to shard daemon at /var/lib/shard/shard.sock: is it running? systemctl status shard
```

`shard daemon` itself is the one exception: it is the process, not a client of one. No verb starts
the daemon: a resident root process is installed on purpose, through the systemd unit in
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

- **The stores**: the images under `${root}/images`, the policies, the secrets and the sandbox
  records. One writer owns them, so they need no lock between processes: the daemon serializes its
  own writes in memory and every client asks it. The value of a secret crosses the socket once, on
  the `PUT`, and is never written anywhere but the secret store, never logged and never listed back.
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

An exec outlives the daemon too, on the guest side alone. `shard-init` holds the guest process, and
the daemon holds only the host end of the stream, so a restart cuts the client off and the command
keeps running inside the sandbox. The client sees its stream end and its exit code is lost; it
cannot attach to that exec again, because reattaching by exec id is not built. A new `shard exec`
answers as soon as the daemon is back.

## Reconcile at start

The daemon checks every record against the substrate after it takes the lock and before it listens,
so no verb ever reads a record the substrate disagrees with. It corrects a record and never deletes
one:

- A record that says `running` with no process becomes `stopped`, and its `stopped_reason` says
  `daemon restarted and found no process`. `shard ls --all` prints the reason beside the state, and
  `shard inspect` carries it in the record. A `start` clears it.
- A record that says `paused` keeps its state while its snapshot holds a checkpoint, because a
  checkpoint is what a paused sandbox has instead of a process, and `resume` still brings it back.
  A paused record whose snapshot is gone becomes `stopped` with the same reason.
- A record that says `stopped` while the substrate holds a live process becomes `running`, with the
  pid the substrate reports, and the exit status of the run that ended is dropped.
- A record that says `created` is left alone: it never ran.
- Host netfilter is the policy of record and nothing re-applied it while the daemon was down, so
  the whole table goes back on once when any sandbox runs.

Each corrected record is one line in the journal. A daemon that cannot check the records refuses to
start rather than serve verbs over state it has not seen; systemd restarts it. A record the daemon
cannot read is named in the log and left as it is, because a record shard cannot read is one it
cannot correct either. A root with no records needs no substrate, so a host without `runsc` still
gets a daemon that answers the reads and the store verbs.

## The API socket

The daemon listens on `${root}/shard.sock`, so `/var/lib/shard/shard.sock` by default. It never
listens on TCP: a network address is `shard serve`, a separate process, described below. The socket is `0660 root:shard` when the host has a `shard` group, else `0600 root`,
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
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/policies
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/policies/web
curl --unix-socket /var/lib/shard/shard.sock -X PUT -d '{"rules":[{"action":"allow","rule":"api.example.com"}]}' http://localhost/v0/policies/web
curl --unix-socket /var/lib/shard/shard.sock -X DELETE http://localhost/v0/policies/web
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/secrets
curl --unix-socket /var/lib/shard/shard.sock -X PUT -d '{"value":"<value>","destinations":["api.example.com"]}' http://localhost/v0/secrets/API_KEY
curl --unix-socket /var/lib/shard/shard.sock -X DELETE 'http://localhost/v0/secrets/API_KEY?force=true'
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/images
curl --unix-socket /var/lib/shard/shard.sock -X POST -d '{"ref":"alpine:3.20"}' http://localhost/v0/images/pull
curl --unix-socket /var/lib/shard/shard.sock -X DELETE 'http://localhost/v0/images/alpine:3.20?force=true'
curl --unix-socket /var/lib/shard/shard.sock -X POST http://localhost/v0/images/prune
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

- `GET /v0/policies` answers `{"policies": [...]}`, and `GET /v0/policies/{name}` one policy, as
  `shard policy ls` and `shard policy show` print them. 404 when the host holds no such policy.
- `PUT /v0/policies/{name}` takes `{"rules": [{"action": "allow"|"deny", "rule": "<destination>"}]}`
  in the order they were given, compiles them, stores the policy and re-applies it at once to every
  sandbox that names it. It answers 200 with the policy. The CLI never parses a rule: the daemon owns
  the grammar. 400 for a name or a rule the host cannot enforce; 500 when the store holds the new
  rules and the host still enforces the old ones, which the message says.
- `DELETE /v0/policies/{name}` answers 204. 404; 409 naming every sandbox that holds it, and there is
  no force: a sandbox with no policy would have no egress rules at all.
- `GET /v0/secrets` answers `{"secrets": [...]}` with the name, the destinations, the placeholder and
  the times, and never a value. Unreadable files come back in `warnings` beside the readable ones,
  which `secret ls` prints on stderr before it exits non-zero.
- `PUT /v0/secrets/{name}` takes `{"value", "destinations", "mock"}` and answers 200 with the record,
  which carries the placeholder and no value. 400 for a name, a destination or an empty value the
  host refuses; 409 when a sandbox holds the placeholder that a new `mock` would change.
- `DELETE /v0/secrets/{name}` answers 204. 404; 409 naming every sandbox that was granted it, unless
  `?force=true`.
- `GET /v0/images` answers `{"images": [...]}` as `shard image ls` prints them; an entry the daemon
  could not read carries its reason in `broken`.
- `POST /v0/images/pull` takes `{"ref"}`, pulls it and answers 200 with the image. 400 for a
  reference that does not parse; 500 when the registry or the unpack failed.
- `DELETE /v0/images/{ref}` takes the whole reference, slashes and all, and answers 200 with a
  `warnings` array of what it could not delete under the store. 404; 409 naming every sandbox that
  references it, unless `?force=true`.
- `POST /v0/images/prune` removes every image no sandbox references and answers
  `{"removed": [...], "warnings": [...]}`. It refuses with 500 rather than guess when a record is
  unreadable, because an image a sandbox needs would be gone.

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
`ForkSandbox`, `CloneSandbox`, `Exec`, `ResizeExec`, `Logs`, `ListPolicies`, `GetPolicy`,
`SetPolicy`, `RemovePolicy`, `ListSecrets`, `SetSecret`, `RemoveSecret`, `ListImages`, `PullImage`,
`RemoveImage` and `PruneImages`, hand-written over the socket. `Exec` dials the socket, writes the request itself and reads the 101,
because `net/http` gives no connection back; `Logs` holds its stream open for as long as the follow
lasts. Neither takes the 30 s deadline the answered-in-full calls take.
The CLI verbs call it and nothing else. Each call that answers in full gets 30 s, per request and
not on the `http.Client`; a daemon that accepts and never answers fails as `GET <route> on <socket>:
no answer within 30s`. `CreateSandbox` sets no deadline, because the pull inside it has none the
client could know, and neither do the four snapshot verbs, because a checkpoint takes as long as
the memory and the disk it writes; `StopSandbox` and `RemoveSandbox` add the grace to theirs.

## The TCP front

`shard serve` is how a client on another host reaches the daemon. It accepts TCP, terminates TLS,
checks `Authorization: Bearer <token>` against the token file, and then dials `${root}/shard.sock`
and copies bytes both ways:

```
shard --root /var/lib/shard serve --listen :2376 \
  --cert /etc/shard/serve.crt --key /etc/shard/serve.key --token-file /etc/shard/serve.token
```

It is a byte proxy and not an API. It reads the request line and the headers of a connection only as
far as the auth header, replays those bytes onto the socket and then splices the two connections, so
an exec upgrade and a `logs` follow pass through untouched and every route above works unchanged. A
bad or missing token is a `401` written before anything is dialed, so an unauthenticated client
never reaches the daemon. The token is compared in constant time, and neither the front nor the CLI
ever logs its value. Without `--cert` and `--key` the front refuses to start: there is no plain TCP
mode to fall back to. The token file must not be readable by everyone on the host, and the front
refuses one that is.

The check is per connection, as a TLS client certificate would be: the token is read once, at the
head of the first request, and the rest of that connection is bytes.

**This is a deliberate deviation from dockerd and hypeman, which bind TCP themselves.** The shard
daemon is root and owns the sandboxes, so the network-facing process is a separate and unprivileged
one. It runs from its own unit, `packaging/systemd/shard-serve.service`, off unless it is installed
on purpose:

```
useradd --system --no-create-home --gid shard shard
install -d -m0750 /etc/shard
openssl rand -hex 32 > /etc/shard/serve.token
chown root:shard /etc/shard/serve.token && chmod 0640 /etc/shard/serve.token
cp packaging/systemd/shard-serve.service /etc/systemd/system/
systemctl enable --now shard-serve
```

The unit runs as `shard:shard`, which is the group the socket is given, and reads the token from a
root-owned `0640` file that the group can read. The account has no other privilege: it cannot read
a state file, and the daemon still applies every rule of every verb.

The CLI reaches a front instead of the socket with three flags, or the environment behind them:

```
shard --host https://box.example.com:2376 --token-file ~/.shard/token --ca-file ~/.shard/ca.pem ls
SHARD_HOST=https://box.example.com:2376 SHARD_TOKEN_FILE=~/.shard/token shard ls
```

`--host` must be an `https` url, and its port defaults to 2376. `--ca-file` names the certificate
that signed the front's own, which a private CA or a self-signed certificate needs; without it the
host's own trust store decides. This is one transport switch inside `services/client` and nothing
else changes: the same typed calls, the same frames, the same errors. It is also the one way a
client off Linux drives sandboxes, because the daemon itself runs on Linux alone.

## One daemon per root

The daemon takes an exclusive flock on `daemon.lock` under the root and refuses to start while
another holds it. It is the only lock shard keeps: the daemon is the single writer of the state, so
nothing else is contended between processes. The lock dies with the process, so there is no stale
pid file to clean up. Nothing probes it to decide whether a daemon is up: a client that needs one
asks the socket and reads the outcome.

## Supervision

Each piece of background work is a task. A task that returns an error is restarted with exponential
backoff and jitter, capped at a minute; a run that stays up long enough earns a fresh backoff, so a
crash loop backs off and a one-off crash restarts fast. A panic in a task is contained and counts as
a failure. A task that returns nil is done and stays done until the daemon restarts. The `api` task
returns an error when its listener dies, so it is restarted, and nil only when the daemon is ending.
