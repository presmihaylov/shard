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
- **The egress proxy**: the `proxy` task listens on the bridge gateway, ports 30080 and 30443, and
  every fronted sandbox's web traffic goes through it. It is restarted like any task after a crash.
- **Egress log rotation**: the `egress-log-rotation` task renames a sandbox's `egress.jsonl` once it
  passes 8 MiB and keeps one file behind it. Without it the log grows without a bound.
- **The liveness loop**: the `liveness` task asks the substrate about every record that says
  `running`, every 5 s, and makes the record agree. It records an entrypoint that exited, stops a
  sandbox whose process is gone, and brings back one the host ended for its memory. See below.
- **The health check**: the `health-check` task runs the probe a sandbox was created with, on the
  interval it named, and keeps the result on the record. See below.
- **The restart policy**: `shard-init` starts the entrypoint again under the policy a sandbox was
  created with, and the `restart-policy` task copies its count onto the record every second. See below.

- **The stores**: the images under `${root}/images`, the policies, the secrets and the sandbox
  records. One writer owns them, so they need no lock between processes: the daemon serializes its
  own writes in memory and every client asks it. The value of a secret crosses the socket once, on
  the `PUT`, and is never written anywhere but the secret store, never logged and never listed back.
  A create pulls the image and writes its sandbox record under the image lock, so `image rm` and
  `image prune`, which both free by reachability over the records, never sweep a rootfs mid create.
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

An exec is a resource the daemon owns for the life of the sandbox. It starts the command at once and
keeps the last 8 MiB of its output, so a client that drops re-attaches by exec id, replays what it
missed and streams the rest. One client attaches at a time. The record holds the exit once the
command ends, `kill` signals it while it runs, and only an `rm` of the exec or a `stop` of the
sandbox frees it. The daemon keeps at most 32 exited execs per sandbox, so a new exec evicts the
oldest exited one and the retained output stays bounded; a running exec never counts. An evicted
exec answers 404, the same as a deleted one.

An exec does not outlive the daemon. The daemon holds the record and the buffer in memory, and
`shard-init` holds the guest process, so a restart cuts every client off and loses the record while
the command keeps running inside the sandbox. A new `shard exec` answers as soon as the daemon is
back.

## Reconcile at start

The daemon checks every record against the substrate after it takes the lock and before it listens,
so no verb ever reads a record the substrate disagrees with. It corrects a record and never deletes
one:

- A record that says `running` with no process becomes `stopped`, and its `stopped_reason` says
  `daemon restarted and found no process`. `shard ls --all` prints the reason beside the state, and
  `shard inspect` carries it in the record. A `start` clears it.
- A record that says `paused` keeps its state while its snapshot holds a checkpoint, because a
  checkpoint is what a paused sandbox has instead of a process, and `resume` still brings it back.
  A paused record whose snapshot is gone becomes `stopped` with the same reason. Only an absent
  checkpoint counts as gone: a read that fails for any other reason refuses the start instead, so
  one bad boot cannot end every future `resume` while the checkpoint sits on disk.
- A record that says `stopped` while the substrate holds a live process becomes `running`, with the
  pid the substrate reports, and the exit status of the run that ended is dropped.
- A record that says `created` is left alone: it never ran.
- A record that says `pending` becomes `running` when the substrate holds its process, because a
  start that took before the daemon stopped did reach `running`. With no process behind it the record
  becomes `failed`, and its `failed_reason` says `the daemon restarted before the create finished`: a
  create the daemon was still running when it stopped never reached `running`, so the record is
  terminal and only `rm` frees it.
- Host netfilter is the policy of record and nothing re-applied it while the daemon was down, so
  the whole table goes back on once when any sandbox runs.

Each corrected record is one line in the journal. A daemon that cannot check the records refuses to
start rather than serve verbs over state it has not seen; systemd restarts it. A record the daemon
cannot read is named in the log and left as it is, because a record shard cannot read is one it
cannot correct either. A root with no records needs no substrate, so a host without `runsc` still
gets a daemon that answers the reads and the store verbs.

## Liveness

Every 5 s the `liveness` task asks the substrate about every record that says `running` and makes
the record agree with it, under the same per-sandbox lock the verbs take. It reads the record again
after the lock, so a `stop` that landed since the list wins and the tick leaves that sandbox alone.
One tick has three outcomes.

The entrypoint exited but the sandbox is still up. The sandbox outlives its entrypoint, so the state
stays `running` and the tick writes the exit into `exit_status`. `shard ls` then shows `running
(exited 7)`, and `shard inspect` shows the code and the signal, so an operator tells a clean exit
from a crash without a `stop`. The exit comes from the file `shard-init` writes, the same one a
`stop` reads (SHARD-168 replaces the channel underneath, not the value). A `stop` that follows uses
the recorded exit and does not wait for it again.

The sandbox process is gone. Nothing but the host or a crash ends a running sandbox behind the
daemon's back, so the tick makes the record `stopped` with `pid` 0 and `stopped_reason` `the sandbox
process died`. A `start` brings it back. The daemon does not start it again on its own; a restart
policy for a process that died is SHARD-188.

The host ended it for its memory. A sandbox that overruns its `--memory` bound is ended by the host,
whole: the kernel kills every process in its cgroup, `shard-init` included, and `runsc` still holds
the dead container. One the host ended for its memory becomes `stopped`, and its `stopped_reason`
says `ran out of memory and the host ended it`; an `exec` on it answers 409 with the same words
until a tick acts on it. A sandbox
created with `--restart-on-oom` is started again instead, as `shard start` would do it: the dead
cgroup is removed, so the bound holds on the next run, the namespace is built again over the same
address, and the entrypoint runs from the beginning. Its memory, its processes and its sockets are
gone, which is why the policy is opt-in and needs a bound: the host never counts an OOM against a
sandbox that has none.

`--restart-on-oom` alone starts it again without end; `--restart-on-oom=N` (`max_oom_restarts` in the
create body, 0 for unlimited) caps the starts in a row at N. A run that lasts ten seconds since the
daemon last started it clears the count first, so a sandbox that only overruns now and then never
spends a finite cap. The record counts the starts in `oom_restarts`, with the last one in
`oom_restarted_at`, and `shard ls` shows the policy in its `RESTART` column as `on-oom 2` when the
count is unlimited, or `on-oom 2/5` under a cap. The second start waits 1 s from the last, then 2, 4
and 8 s, up to 60 s. At the cap the sandbox stays `stopped` and the reason adds `the N starts again
the limit allows are spent`. A `shard start` by hand still works, and clears the reason. A sandbox
the record says `stopped` is never started again by the daemon, so a `stop` in the window is final.
A `fork` or `clone` inherits the policy with a fresh count.

## Health check

A sandbox created with a probe is probed by the daemon while it runs, and the record says what the
probes found. `--health-command <cmd>` (`"health": {"command": [...]}` in the
create body, where the flag wraps its string as `/bin/sh -c`) runs the argv in the sandbox through
the provider's `exec` and passes on exit 0; a signal or a command that could not start fails.
`--health-interval`, `--health-timeout` and `--health-retries` (`interval`, `timeout`,
`retries` in the body, whole seconds like the `grace` of a stop) default to 30 s, 10 s and 3.

The record carries the probe in `health_check`, with every default filled in, and the result in
`health`: `{"status", "checked_at", "failures"}`. The status is `starting` until the first probe
passes, `healthy` from then on, and `unhealthy` once `retries` probes in a row have failed; one that
passes sets it back to `healthy` and clears the count. `checked_at` is the last probe. Both are
absent on a sandbox that has no probe. `shard ls` shows the status in its `HEALTH` column, with the
count beside it while it is above zero, as `unhealthy 3/3`; each change of status is one line in
the daemon log, with the reason for a failure.

The task ticks every second, probes every due sandbox side by side and waits for the slowest, so a
probe runs at most one timeout late. A command probe that outruns its timeout is ended inside the
sandbox. There is no start period: the first probe runs on the first tick after the
create, and a slow entrypoint counts its failures from the start, so set `retries` and `interval`
for it. Every new run starts over at `starting`: a `start`, a start again after an OOM, a `clone`
and a daemon that finds the sandbox running at reconcile. A `resume` and a `fork` keep the result,
as they keep the process. Nothing acts on `unhealthy` yet; the status is for the operator and the
API, and a later task set may restart on it.

## Restart policy

A sandbox outlives its entrypoint, and the policy does not change that: it starts the entrypoint
again inside the sandbox that is already up, and the sandbox stays `running` whatever the policy
does. `--restart <policy>` (`"restart": {"policy"}` in the create body) is `no`, the default,
`on-failure`, which starts the entrypoint again after an exit other than 0 or a signal, or
`always`, which does so after every exit. `--restart-retries` (`retries`) caps the starts again in
one run; with no cap `on-failure` starts it again without end, and `always` never gives up and takes
no retries at all. `--restart-backoff` (`backoff`, whole seconds, 1) is the wait before the first
start again; it doubles each time, up to 60 s. Both need a policy. A run that lasts ten seconds since
its last start clears the count, so a slow crash loop never spends a finite cap. At the cap
`on-failure` gives up and the entrypoint stays exited, and a stop puts its last exit in `exit_status`
as after any exit. A stop during the wait ends the sandbox at once and drops the start that was due.

The policy is fixed at create. `shard-init` gets it as flags in the bundle and has no control
channel, so nothing changes it on a sandbox that runs. `shard-init` counts every start again in a
file beside the exit file, and the `restart-policy` task reads that file every second for every
running sandbox that has a policy and copies it onto the record, so the record is at most a second
behind, and a `stop` reads the count once more before it writes the stopped record. The record
carries `restart`: `{"policy", "retries", "backoff", "count", "last_at", "gave_up"}`, absent on a
sandbox without a policy, and `retries` is omitted when the count is unlimited. `shard ls` shows it in
the `RESTART` column beside the OOM policy, as `on-failure 2` when unlimited, `on-failure 2/5` under a
cap, then `on-failure 5/5 gave up`, and `always 7`; each start again and the give-up are one line in
the daemon log. The count is for one run and `shard-init` never clears it: a `start` of a stopped
sandbox begins a new run at zero, a `resume` and a `fork` keep the count with the process, and a
`clone` inherits the policy alone. This is a convenience for a flaky entrypoint, not a lifetime
mechanism: `stop` stays the only thing that ends a sandbox.

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
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/daemon
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/sandboxes
curl --unix-socket /var/lib/shard/shard.sock 'http://localhost/v0/sandboxes?all=true'
curl --unix-socket /var/lib/shard/shard.sock 'http://localhost/v0/sandboxes?limit=20&cursor=<id>'
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/sandboxes/<id or name>
curl --unix-socket /var/lib/shard/shard.sock -X POST -d '{"image":"alpine:3.20","command":["sleep","600"]}' http://localhost/v0/sandboxes
curl --unix-socket /var/lib/shard/shard.sock -X POST http://localhost/v0/sandboxes/<id or name>/start
curl --unix-socket /var/lib/shard/shard.sock -X POST -d '{"grace":10}' http://localhost/v0/sandboxes/<id or name>/stop
curl --unix-socket /var/lib/shard/shard.sock -X DELETE 'http://localhost/v0/sandboxes/<id or name>?force=true&grace=10'
curl --unix-socket /var/lib/shard/shard.sock -X POST http://localhost/v0/sandboxes/<id or name>/pause
curl --unix-socket /var/lib/shard/shard.sock -X POST http://localhost/v0/sandboxes/<id or name>/resume
curl --unix-socket /var/lib/shard/shard.sock -X POST -d '{"name":"web-2"}' http://localhost/v0/sandboxes/<id or name>/fork
curl --unix-socket /var/lib/shard/shard.sock -X POST -d '{"command":["/bin/echo","hi"]}' http://localhost/v0/sandboxes/<id or name>/exec
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/sandboxes/<id or name>/exec
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/sandboxes/<id or name>/exec/<exec-id>
curl --unix-socket /var/lib/shard/shard.sock -X POST -d '{"signal":"TERM"}' http://localhost/v0/sandboxes/<id or name>/exec/<exec-id>/kill
curl --unix-socket /var/lib/shard/shard.sock -X DELETE http://localhost/v0/sandboxes/<id or name>/exec/<exec-id>
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/sandboxes/<id or name>/logs
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/sandboxes/<id or name>/egress-log
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
- `GET /v0/daemon` answers what the daemon knows about itself: `version`, `pid`, `started_at`,
  `socket`, `provider`, `capabilities` as the provider's three booleans (`pause`, `resume`, `fork`)
  and `proxy` with `plain_port` and `tls_port`. `shard daemon status` prints it, one field per line.
  The provider is built on the first ask, so on a host without its runtime the route answers 500
  and says what is missing.
- Every list answers `{"<plural>": [...], "next": null | "<cursor>"}`, plus `warnings` where the
  route says so. `?limit=N` caps the page and `?cursor=<c>` serves the items whose key sorts after
  the cursor, in the order of the list; `next` of the page before is the key of its last item: the id
  of a sandbox, the name of a policy or a secret, the reference of an image. A cursor is a position,
  not an item, so a sandbox removed between two pages does not break the walk. Without `limit` the
  list is whole and `next` is `null`; with one, `next` is `null` only once nothing follows. A `limit`
  under 1, or a cursor that could never be a key of the list, is 400 `invalid_request`. The CLI never
  sets a limit, so `ls` and the store lists print everything.
- `GET /v0/sandboxes` answers `{"sandboxes": [...], "next"}` as `shard ls` lists them: stopped
  sandboxes hidden unless `all=true`. When some records are unreadable it still answers 200 with the
  readable sandboxes and a `warnings` array, one string per unreadable record; `ls` prints the table
  and then the warnings on stderr, and exits non-zero. It answers 500 only when the list itself
  failed.
- `GET /v0/sandboxes/{id}` takes an id or a name and answers the record, with an `egress` object
  beside it when the record names a policy: what the host enforces, as `shard inspect` prints it.
  With `?wait=true` it holds until the record leaves `pending`, so a caller reads the `running` or
  `failed` a background create reached rather than the `pending` it started; a record that is not
  pending answers at once. 404 when nothing has it, which `inspect` prints as `no sandbox <ref>`;
  400 when the reference does not validate; 500 for anything else: an unreadable record, a name link
  at something that is not a sandbox, or a policy that cannot be compiled.

- `POST /v0/sandboxes` takes `{"image", "name", "command", "env", "workdir", "user", "secrets",
  "policy", "resources": {"memory_mib", "vcpus"}, "restart_on_oom", "max_oom_restarts", "health": {"command",
  "interval", "timeout", "retries"}, "restart": {"policy", "retries",
  "backoff"}}` and answers 201 with the record. A cached image needs no pull, so the create builds
  and starts the sandbox before it answers and the record says `running`; a claim that fails then
  gives everything back and answers 500. An uncached image makes the record `pending` and answers
  before the download: the daemon pulls, builds and starts behind it, and the record lands `running`
  or `failed` with a one-line `failed_reason`, so a background pull or start that fails is read from
  the record, not an error. With `?wait=true` the create holds until the record leaves `pending` and
  answers the `running` or `failed` it reached, so a caller reads the settled record without a poll;
  the plain create answers at once. 400 when the body does not decode or a field does not validate, or
  when it names a secret or a policy the host does not hold; 409 `name_taken` when another sandbox
  already holds the name.
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
  "size": {"rows", "cols"}}`, validates it, starts the command at once and answers 201 with the exec
  record: `{"exec", "sandbox", "command", "state": "running"|"exited", "exit_status": {"code",
  "signal"} or null, "started_at", "exited_at", "truncated"}`. 400 for a body that does not decode or
  a request that names no command; 404; 409 when no command can run in the sandbox.
- `GET /v0/sandboxes/{id}/exec` answers `{"execs": [...], "next"}` with every exec the sandbox holds.
- `GET /v0/sandboxes/{id}/exec/{exec-id}` answers the exec record. With `?wait=true` it holds the
  answer until the command ends, then answers the ended record. With the WebSocket handshake it
  answers 101 instead and attaches: it replays the buffered output, then streams live to the exit on
  stream 3. 404 when the exec ended with the sandbox, was evicted by the 32-exec cap, or belongs to
  another; 409 `in_use` for a second attach. A drop leaves the command running, so a later attach
  replays it again.
- `POST /v0/sandboxes/{id}/exec/{exec-id}/kill` takes `{"signal": "TERM"|"KILL"}`, the default being
  TERM, signals the running command and answers 204. 404; 409 `exec_exited` once the command ended.
- `DELETE /v0/sandboxes/{id}/exec/{exec-id}` answers 204 and frees the record and its buffer. 404;
  409 `exec_running` while the command still runs.
- `POST /v0/sandboxes/{id}/exec/{exec-id}/resize` takes `{"rows", "cols"}` and answers 204. 404 when
  the exec has no terminal, has ended or belongs to another sandbox. Only a `tty` exec has a terminal
  to resize.
- `GET /v0/sandboxes/{id}/logs` answers 200 `text/plain; charset=utf-8` with everything the
  entrypoint wrote. 404; 400 for a `follow` that is not a boolean.
- `GET /v0/sandboxes/{id}/logs?follow=true` with the WebSocket handshake is binary messages, the log
  bytes on stream 1, then `{"reason": "stopped"|"removed"}` on stream 3 when the sandbox is gone and
  the daemon closes with 1000, or the failure on stream 5. Without the handshake it is 200 chunked
  `text/plain`, the bytes as they come, and the body ends when the sandbox stops or is removed, so
  `curl -N` follows a log. 404 either way, before anything is on the wire. `shard logs -f` takes the
  WebSocket.
- `GET /v0/sandboxes/{id}/egress-log` answers 200 with the egress decisions of the sandbox as a JSON
  array, oldest first: the proxy's own records and the host drops the daemon wrote into the same file.
  404. `shard logs --egress` prints one record per line.
- `GET /v0/sandboxes/{id}/egress-log?follow=true` with the handshake is text messages, one JSON record
  each, live. A stopped or removed sandbox ends it with close 1000 and the reason as the close text; a
  failure of the follow is close 1011 with the error. Without the handshake it is 200 chunked
  `application/x-ndjson`, one record per line as it lands, and the body ends on the same stop or rm.
  404 either way, before anything is on the wire.
- `POST /v0/sandboxes/{id}/secrets/{name}` grants a stored secret to a created or stopped sandbox and
  answers 200 with the record: the placeholder lands in the bundle environment, the proxy CA in the
  writable layer. 404; 400 when the host holds no such secret, or when the guest environment already
  holds that name; 409 when the sandbox runs or is paused.
- `DELETE /v0/sandboxes/{id}/secrets/{name}` takes the grant and the placeholder back and answers 200
  with the record. The proxy CA stays. 404; 409 when the sandbox runs or is paused.
- `PUT /v0/sandboxes/{id}/policy` takes `{"policy": "<name>"}` and gives a created or stopped sandbox
  that policy, answering 200 with the record: the record is written, the host rules are applied, and a
  host that refuses them puts the record back. A sandbox holds one policy, so this replaces the one it
  holds. 404; 400 when the host holds no such policy; 409 when the sandbox runs or is paused.
- `DELETE /v0/sandboxes/{id}/policy` leaves the sandbox with no policy and answers 200 with the record.
  The secrets it holds and the proxy CA stay. 404; 409 when the sandbox runs or is paused.

- `GET /v0/policies` answers `{"policies": [...], "next"}`, and `GET /v0/policies/{name}` one policy with
  `holders`, the sandboxes whose record names it, omitted when none does. That is what `shard policy
  ls` and `shard policy show` print. 404 when the host holds no such policy.
- `PUT /v0/policies/{name}` takes `{"rules": [{"action": "allow"|"deny", "rule": "<destination>"}]}`
  in the order they were given, compiles them, stores the policy and re-applies it at once to every
  sandbox that names it. It answers 200 with the policy. The CLI never parses a rule: the daemon owns
  the grammar. 400 for a name or a rule the host cannot enforce; 500 when the store holds the new
  rules and the host still enforces the old ones, which the message says.
- `DELETE /v0/policies/{name}` answers 204. 404; 409 naming every sandbox that holds it, and there is
  no force: a sandbox with no policy would have no egress rules at all.
- `GET /v0/secrets` answers `{"secrets": [...], "next"}` with the name, the destinations, the placeholder and
  the times, and never a value. Unreadable files come back in `warnings` beside the readable ones,
  which `secret ls` prints on stderr before it exits non-zero.
- `PUT /v0/secrets/{name}` takes `{"value", "destinations", "mock"}` and answers 200 with the record,
  which carries the placeholder and no value. 400 for a name, a destination or an empty value the
  host refuses; 409 naming every sandbox that holds the placeholder a new `mock` would change.
- `DELETE /v0/secrets/{name}` answers 204. 404; 409 naming every sandbox that was granted it, unless
  `?force=true`.
- `GET /v0/images` answers `{"images": [...], "next"}` as `shard image ls` prints them; an entry the daemon
  could not read carries its reason in `broken`.
- `POST /v0/images/pull` takes `{"ref"}`, pulls it and answers 200 with the image. 400 for a
  reference that does not parse; 500 when the registry or the unpack failed.
- `DELETE /v0/images/{ref}` takes the whole reference, slashes and all, and answers 200 with a
  `warnings` array of what it could not delete under the store. 404; 409 naming every sandbox that
  references it, unless `?force=true`.
- `POST /v0/images/prune` removes every image no sandbox references and answers
  `{"removed": [...], "warnings": [...]}`. It refuses with 500 rather than guess when a record is
  unreadable, because an image a sandbox needs would be gone.

A stream is a WebSocket (RFC 6455) on the same route, opened with the standard handshake. Every
refusal comes before the 101 as a status and a JSON body. An exec carries binary messages whose
first byte is the stream and the rest the payload: the client sends 0 (stdin) and 4 (stdin closed,
empty); the daemon sends 1 (stdout), 2 (stderr), 3 (exit, `{"code", "signal"}`, plus `"error"` when
the command never ran) and 5 (a failure of the daemon's own, `{"error": {"code", "message"}}`, the
same object every error body carries). One payload is at most 1 MiB, and a longer write goes as several messages. 3 or 5 ends the
session and the daemon closes with 1000; a client that closes first kills the command. A `tty` exec
carries the guest's terminal on stream 1 alone, because a terminal has no second stream to keep
apart. Ping and pong are the standard ones. The two `?follow=true` routes also serve without the
handshake, as a chunked body that ends with the sandbox, so `curl -N` follows either; an exec
attach does not.

A 409 body is the refusal as the CLI prints it: `sandbox <id> is <state>: <fix>`. A verb the
provider does not claim is a 409 too: `provider <name> does not support <verb> on this host`.

Every error body is `{"error": {"code": "<code>", "message": "<message>"}}`: `message` is the
line the CLI prints, `code` is what a program matches on, and nothing else is ever in the table.
Whatever else a refusal carries lives inside `error`, and nothing else is ever at the root.

| code | status | when |
|---|---|---|
| `invalid_request` | 400 | the body does not decode, a field does not validate, or a named secret, policy or image is unknown |
| `not_found` | 404 | no sandbox, policy, secret, image or exec has the reference, or no route has the path |
| `sandbox_not_running` | 409 | exec or pause on a sandbox that is not running, or one the substrate no longer holds |
| `sandbox_not_stopped` | 409 | start, clone, or rm without force on a sandbox that is up |
| `sandbox_not_paused` | 409 | resume on a sandbox that is not paused |
| `sandbox_failed` | 409 | any verb but a get or an `rm` on a create that ended `failed`; the message carries the `failed_reason`, and `rm` frees it |
| `sandbox_live` | 409 | grant, ungrant, attach or detach while the sandbox runs or is paused |
| `no_snapshot` | 409 | resume or fork when the record names no snapshot |
| `unsupported` | 409 | the provider does not claim the verb |
| `in_use` | 409 | delete a policy, secret or image that sandboxes hold, or move the placeholder of a secret they hold; `error` adds `"holders": [ids]`. Also a second attach of an exec, with no holders |
| `name_taken` | 409 | a create whose `name` another sandbox already holds |
| `websocket_required` | 400 | an exec attach without the WebSocket handshake |
| `unauthorized` | 401 | the TCP front, when the request carries no valid bearer token; nothing is dialed |
| `forbidden` | 403 | the TCP front, when the token is valid but its scopes do not reach the route; nothing is dialed |
| `internal` | 500 | anything else, and the message says what the daemon got back |

`services/client` decodes that object alone into `*client.APIError`, with `Status`, `Code`, `Message`
and `Holders`, so a caller matches on the code with `errors.As` and never on the text; a body of
any other shape is quoted as it came, under `internal`.

The base path is `/v0`, and `/v0` may change until launch 1. SHARD-83 freezes the contract as `/v1`.

The typed side of these routes is `services/client`: `Version`, `ListSandboxes`, `GetSandbox`,
`CreateSandbox`, `StartSandbox`, `StopSandbox`, `RemoveSandbox`, `PauseSandbox`, `ResumeSandbox`,
`ForkSandbox`, `CloneSandbox`, `Exec`, `ResizeExec`, `Logs`, `ListPolicies`, `GetPolicy`,
`SetPolicy`, `RemovePolicy`, `ListSecrets`, `SetSecret`, `RemoveSecret`, `ListImages`, `PullImage`,
`RemoveImage` and `PruneImages`, hand-written over the socket. `Exec` creates the exec and then
opens the WebSocket over the same socket; `Logs` with follow and `FollowEgressLog` hold theirs open
for as long as the follow lasts. A stream takes no deadline; the create before it does.
The CLI verbs call it and nothing else. Each call that answers in full gets 30 s, per request and
not on the `http.Client`; a daemon that accepts and never answers fails as `GET <route> on <socket>:
no answer within 30s`. `CreateSandbox` sets no deadline, because the pull inside it has none the
client could know, and neither do the four snapshot verbs, because a checkpoint takes as long as
the memory and the disk it writes; `StopSandbox` and `RemoveSandbox` add the grace to theirs.

## The TCP front

`shard serve` is how a client on another host reaches the daemon. It accepts TCP, terminates TLS,
verifies the JWT in `Authorization: Bearer <jwt>` against a signing secret, and then dials
`${root}/shard.sock` and copies bytes both ways:

```
shard --root /var/lib/shard serve --listen :2376 \
  --cert /etc/shard/serve.crt --key /etc/shard/serve.key --secret-file /etc/shard/serve.secret
```

It is a byte proxy and not an API. It reads the request line and the headers of a request only as
far as the auth header, replays those bytes onto the socket and then splices the two connections, so
the WebSocket handshake of an exec, a `logs` follow or an `egress-log` follow, and every message
after it, pass through untouched and every route above works unchanged. A bad or missing token is a
`401` with the code `unauthorized`, written before anything is dialed, so an unauthenticated client
never reaches the daemon. A socket that does not answer is a `502` with the code `internal`. The
front verifies HS256 alone: a token signed by another algorithm, a token signed by another secret, a
token with no subject, a token with no expiry and an expired token are each the same `401`. Neither
the front nor the CLI ever logs a token or the secret, and the front logs the subject of every
request it lets through. Without `--cert` and `--key` the front refuses to start: there is no plain
TCP mode to fall back to. The secret file must not be readable by everyone on the host, and the front
refuses one that is.

The access control is TLS on the wire, one signing secret in a file, and a coarse scope on each
token. There is no user and no role yet.

A token carries a list of scopes, and the front maps each request's route to one capability and lets
the request through only when a scope covers it. A token with no scopes, or one that carries `*`,
holds every verb; a token that names scopes reaches only the routes those scopes cover, and every
other route is a `403` with the code `forbidden`, written before anything is dialed. The front maps
the request line to the capability over the daemon's own route patterns, so the front and the daemon
agree on what each request is, and an unknown route is a `403` too. The eight capabilities are:

| capability | routes |
| --- | --- |
| `daemon:read` | `GET /v0/version`, `GET /v0/daemon` |
| `sandbox:read` | list, get, `logs` and `egress-log` |
| `sandbox:write` | create, start, stop, pause, resume, fork and clone |
| `sandbox:delete` | `rm` |
| `exec` | every `exec` route |
| `image:*` | every `images` route |
| `secret:*` | every `secrets` route, and the grant and ungrant on a sandbox |
| `policy:*` | every `policies` route, and the policy of a sandbox |

The check is per request, not per connection: the front forces `Connection: close` on every request
it forwards, so a kept-alive connection cannot carry a second request past the one it verified. A
WebSocket upgrade is the one exception, because its `Connection: Upgrade` is what makes the handshake
work, and the daemon owns the lifetime of that connection once the front splices it. The exception is
narrow: the front skips the `Connection: close` rewrite only when all four handshake headers are
present, and it reads the daemon's status line first. A `101` is the connection the WebSocket needs; a
non-101 answer ends after that one response, so a request pipelined behind a handshake the daemon does
not upgrade never reaches the daemon.

A token is minted on the server, from the same secret, and never over the API:

```
shard serve mint --name ci --duration 24h --secret-file /etc/shard/serve.secret
shard serve mint --name reader --scopes sandbox:read,exec --secret-file /etc/shard/serve.secret
```

`mint` prints one token to stdout and exits. It is a local verb like `daemon` and `serve`: it never
reaches the daemon, and the daemon never sees the secret. `--name` is the subject the front logs,
`--duration` defaults to 24h, and `--scopes` is a comma-separated list of the scopes the token
carries; an empty `--scopes` is every verb. Rotate the secret and every token it signed stops
verifying at once.

The front reads the secret file once, at start, so a rotation needs a `shard serve` restart, and that
restart ends no connection that is already spliced.

**This is a deliberate deviation from dockerd and hypeman, which bind TCP themselves.** The shard
daemon is root and owns the sandboxes, so the network-facing process is a separate and unprivileged
one. It runs from its own unit, `packaging/systemd/shard-serve.service`, off unless it is installed
on purpose:

```
useradd --system --no-create-home --gid shard shard
install -d -m0750 /etc/shard
openssl rand -hex 32 > /etc/shard/serve.secret
chown root:shard /etc/shard/serve.secret && chmod 0640 /etc/shard/serve.secret
cp packaging/systemd/shard-serve.service /etc/systemd/system/
systemctl enable --now shard-serve
```

The unit runs as `shard:shard`, which is the group the socket is given, and reads the secret from a
root-owned `0640` file that the group can read. The account has no other privilege: it cannot read
a state file, and the daemon still applies every rule of every verb.

The CLI reaches a front instead of the socket with three flags, or the environment behind them:

```
shard --remote https://box.example.com:2376 --token-file ~/.shard/token --ca-file ~/.shard/ca.pem ls
SHARD_REMOTE=https://box.example.com:2376 SHARD_TOKEN_FILE=~/.shard/token shard ls
```

`--remote` must be an `https` url, and its port defaults to 2376. `--ca-file` names the certificate
that signed the front's own, which a private CA or a self-signed certificate needs; without it the
host's own trust store decides. This is one transport switch inside `services/client` and nothing
else changes: the same typed calls, the same messages, the same errors. It is also the one way a
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
a failure. A task that returns nil is done and stays done until the daemon restarts. The `api` and
`proxy` tasks return an error when a listener dies, so they are restarted, and nil only when the
daemon is ending.
