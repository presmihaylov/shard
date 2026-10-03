# The daemon

`shard daemon` is the resident process that owns the state. The CLI is a thin client of it: every
verb goes over the socket, none of them reads or writes the state itself, and each fails fast
without a daemon, with one line:

```
shard: cannot connect to shard daemon at /var/lib/shard/shard.sock: is it running? systemctl status shard
```

On a Mac the unit is the LaunchDaemon, so the hint there is `launchctl print system/shard.daemon`.
The unit serves `/var/lib/shard` only, so under any other `--root` the hint names that root's own
daemon instead, `is it running? shard --root /srv/shard-e2e daemon`, and under `--remote` it names
the front, `shard serve on box.example.com:2376`.

`shard daemon` itself is the one exception: it is the process, not a client of one. No verb starts
the daemon: a resident root process is installed on purpose, through the systemd unit in
`packaging/systemd/shard.service`:

```
cp packaging/systemd/shard.service /etc/systemd/system/
systemctl enable --now shard
```

On a Mac the same shape is the LaunchDaemon in `packaging/launchd`, installed as `docs/mac.md` says.

## What the daemon owns

- **The API socket**: the REST surface under `${root}/shard.sock`, described below.
- **The egress proxy**: the `proxy` task listens on the bridge gateway, ports 30080 and 30443, and
  every fronted sandbox's web traffic goes through it. It is restarted like any task after a crash.
- **Output log rotation**: a sandbox's `output.log` and a VM's `console.log` keep 16 MiB each,
  with one older file of up to 16 MiB beside them as `<file>.1`, which `shard logs` prints first.
  The daemon writes a VM's `output.log` itself and renames it before it passes 16 MiB, so that
  bound is exact. runsc and runc hold their `output.log` and the VMM holds `console.log`, so the
  `held-log-rotation` task copies the last 16 MiB of each to `.1` and truncates it in place, every
  second and at once on a start. That bound is soft by one second of output, nothing bounds a log
  while the daemon is down, and what a sandbox writes between the copy and the truncate is lost.
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
  A create writes its sandbox record before it pulls, and a cached pull waits for a removal in
  flight, so `image rm` and `image prune`, which both free by reachability over the records, never
  sweep a rootfs mid create.
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
missed and streams the rest. One client attaches at a time, and the command waits for it rather than
evict output it has not taken; a client that takes nothing for 30 s (`ExecStallBound`) is detached,
the command runs on, and `lost_bytes` counts what no client read. The record holds the exit once the
command ends, `kill` signals it while it runs, and only an `rm` of the exec or a `stop` of the
sandbox frees it. The daemon keeps at most 32 exited execs per sandbox, so a new exec evicts the
oldest exited one and the retained output stays bounded; a running exec never counts. An evicted
exec answers 404, the same as a deleted one.

An exec does not outlive the daemon. The daemon holds the record and the buffer in memory, and
`shard-init` holds the guest process, so a restart cuts every client off and loses the record while
the command keeps running inside the sandbox. The `runsc exec` driver dies with the daemon, and the
next daemon sweeps the scratch every exec keeps under `<root>/exec` before it serves. A new
`shard exec` answers as soon as the daemon is back.

## The data dir on Firecracker

Firecracker copies a disk where gVisor copies an overlay layer, so a `fork` or a `clone` on it is
fast only where the filesystem clones a file by sharing its blocks: XFS with `reflink=1`, or Btrfs.
Before it takes the lock, a daemon with `--provider firecracker` probes its root with one real clone.
A root that clones is left alone. A root that does not, ext4 on most hosts, gets a loopback XFS image
beside it, `<root>.xfs`, formatted with `mkfs.xfs -m reflink=1`, mounted over the root with `-o loop`,
and given a line in `/etc/fstab` so it comes back on boot. The image takes half the free space of the
filesystem that holds it, at most 100 GiB, and its size is fixed once it exists. Every step skips what
is already done, so a restart is a no-op.

The daemon refuses rather than provisions, and says why and what to do, when it is not root, when
`mkfs.xfs` (xfsprogs) is missing, when the root is already a mount that cannot clone, when the root
holds entries the mount would hide, when a file at `<root>.xfs` is not an XFS image, or when half the
free space is under 10 GiB. It never falls back to a full copy. No other provider provisions or
probes anything (SHARD-264).

To undo the bootstrap by hand, first stop every sandbox with `shard stop`, then stop the daemon
(`systemctl stop shard`) and keep it stopped. A sandbox outlives the daemon, and a live VM or daemon
holds files under the mount, so an unmount fails busy and a lazy one detaches live state. Then unmount
the image from the root, remove its `/etc/fstab` line, and delete `<root>.xfs`, `<root>.xfs.lock` and
the root. The daemon writes the line as `<root>.xfs <root> xfs loop,nofail 0 0` and escapes each path
the way `getmntent` reads it: `\134` for a backslash, `\040` for a space, `\011` for a tab and `\012`
for a newline, so a grep for a root with a space in it must search for `\040`. Remove the fstab line
before the next boot, or it mounts the image again.

## Reconcile at start

The daemon checks every record against the substrate after it takes the lock and before it listens,
so no verb ever reads a record the substrate disagrees with. It corrects a record and never deletes
one:

- A record that says `running` with no process becomes `stopped`, and its `stopped_reason` says
  `daemon restarted and found no process`. `shard ls --all` prints the reason beside the state, and
  `shard inspect` carries it in the record. A `start` clears it.
- A record that says `running` whose sandbox the host ended for its memory while the daemon was
  down gets the decision the liveness tick makes for an OOM the daemon saw: it becomes `stopped`
  with `ran out of memory and the host ended it`, or the daemon starts it again when the record set
  `restart_on_oom` and the limit allows. The tick's backoff does not apply here, so no verb ever
  reads it as `running` with no process behind it (SHARD-311).
- A record that says `paused` keeps its state while its snapshot holds a checkpoint, because a
  checkpoint is what a paused sandbox has instead of a process, and `resume` still brings it back.
  A paused record whose snapshot is gone becomes `stopped` with the same reason. Only an absent
  checkpoint counts as gone: a read that fails for any other reason leaves the record `paused`, so
  one bad boot cannot end every future `resume` while the checkpoint sits on disk.
- A record that says `stopped` while the substrate holds a live process becomes `running`, with the
  pid the substrate reports, and the exit status of the run that ended is dropped.
- A record that says `created` becomes `failed`, and its `failed_reason` says `the daemon restarted
  before the fork or clone finished`. No verb leaves a record in `created`: only a fork or clone's
  copy passes through it, and the caller got an error, not the id. A copy whose process still runs is
  stopped first, because `rm` refuses a live sandbox and `stop` refuses a failed one. Then the daemon
  tears down the copy's substrate, as `rm` does, because a restore the old daemon started can run on
  where the runtime cannot see it. On gVisor that teardown first kills any `runsc restore` of the
  copy, because until it starts the sandbox it is outside the sandbox's cgroup, and the daemon lock
  means no new one can start. A fork or resume records the binary and the command line of its restore
  in `restore.json` before it runs, so the kill finds the restore even after runsc was replaced, and
  it signals through a pidfd, so a pid reused in between is never hit. A copy with no `restore.json`,
  from a daemon older than the file, matches a process the daemon's user started with the restore
  command line on the copy's own bundle, from any snapshot and any runsc binary. A teardown that
  fails leaves the record as it is, with a line in the log, and the next start of the daemon tries
  again.
- A record that says `pending` becomes `running` when the substrate holds its process, because a
  start that took before the daemon stopped did reach `running`. With no process behind it the record
  becomes `failed`, and its `failed_reason` says `the daemon restarted before the create finished`: a
  create the daemon was still running when it stopped never reached `running`, so the record is
  terminal and only `rm` frees it.
- Host netfilter is the policy of record and nothing re-applied it while the daemon was down, so
  the whole table goes back on once when any sandbox runs.

Each corrected record is one line in the journal. A record the daemon cannot check, because the
substrate refused the probe or a write failed on a full root, is one line that names the sandbox
and the error, and the record is left as it is. A daemon that refused to start could not serve the
other sandboxes, nor `rm` the one that fills the root (SHARD-341). A daemon that cannot list the
records or put the host rules back refuses to start; systemd restarts it. A record the daemon
cannot read is named in the log and left as it is, because a record shard cannot read is one it
cannot correct either. The vz and Firecracker providers write the initrd and a replayed restart
count only when the bytes on disk differ, so a start on a full root writes nothing it already has.
A root with no records needs no substrate, so a host without `runsc` still gets a daemon that
answers the reads and the store verbs.

## A full root

A daemon that cannot start on a full root cannot serve the `rm` that frees it, so it holds 64 MiB
back in `<root>/.reserve` (SHARD-351). It writes the file under the lock, as real blocks, and only
when the root has at least four times that free; a fuller root starts without one and logs a line.

Every write a start makes goes through one retry: the reflink probe of the data dir, the initrd of
the vz and Firecracker providers, the reconcile record writes and the socket bind. When a step
fails with `ENOSPC`, the daemon deletes `.reserve`, runs the step once more at once and logs one
line with the step and the free space before and after. A second `ENOSPC` fails the step. The next
start with room writes the reserve again.

Known limit: on macOS, a Time Machine local snapshot of the Data volume can keep the blocks of a
deleted file, so the delete gives back less than the reserve. The log line says how much less.

## Liveness

Every 5 s the `liveness` task asks the substrate about every record that says `running` and makes
the record agree with it. It asks under a 10 s deadline and outside the per-sandbox lock, so a
substrate call that wedges never pins that lock: a `stop` or `rm` on that sandbox, or on any other,
still runs. A tick the substrate does not answer within the deadline logs one line, leaves the record
untouched, and asks again next tick. The tick takes the lock only to write, and reads the record
again first, so a `stop` that landed since the list wins and the tick leaves that sandbox alone.
One tick the substrate answers has four outcomes.

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

`shard-init` itself died. On Firecracker `shard-init` tells the host why before the VM halts, with
the exit code 125, as `docs/provider-vz.md` says. The tick makes the record `stopped` with `pid` 0,
`exit_status` 125 and `stopped_reason` `shard-init failed: <reason>`, and logs one line. A `start`
brings it back and clears both. A run whose last report never landed ends the same way, with the
loss as the reason, but its `start` answers with the loss until `rm` drops the sandbox.

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

`--restart-on-oom` alone starts it again without end; `--restart-on-oom=N` (`max_oom_restarts` in
the create body, 0 for unlimited) caps the starts in a row at N. A run the daemon has seen ten
seconds in a row under its memory throttle clears the count first, so a sandbox that only overruns
now and then never spends a finite cap, and one held at the throttle until the host ends it always
does. The throttle is `memory.high`, which only gVisor sets; on the other providers this is ten
seconds since the start. The daemon looks every 5 s and keeps what it saw in the record, so a daemon
restart keeps it. Host page cache counts against the throttle too, so heavy file reads near the
bound also keep a run from counting as calm. The record counts the starts in `oom_restarts`, with
the last one in `oom_restarted_at`, and `shard ls` shows the policy in its `RESTART` column as
`on-oom 2` when the count is unlimited, or `on-oom 2/5` under a cap. The second start waits 1 s from
the last, then 2, 4 and 8 s, up to 60 s. At the cap the sandbox stays `stopped` and the reason adds
`the N starts again the limit allows are spent`. A `shard start` by hand still works, and clears the
reason. While a start again waits, the record says `stopped` with pid 0, the reason adds
`it starts again at <time>`, and `oom_restart_due` holds that time, so no verb reads the dead
process (SHARD-425); `shard ls` still lists it. A `stop` in the wait calls the start again off, and
a `shard start` runs it at once. A `fork` or `clone` inherits the policy with a fresh count.

## Health check

A sandbox created with a probe is probed by the daemon while it runs, and the record says what the
probes found. `--health-command <cmd>` (`"health": {"command": [...]}` in the
create body, where the flag wraps its string as `/bin/sh -c`) runs the argv in the sandbox through
the provider's `exec` and passes on exit 0; a signal or a command that could not start fails.
`--health-interval`, `--health-timeout` and `--health-retries` (`interval`, `timeout`,
`retries` in the body, whole seconds like the `grace` of a stop) default to 30 s, 10 s and 3. A
create refuses an interval over 3600 s or a timeout over 600 s, and the error names the bound.

The record carries the probe in `health_check`, with every default filled in, and the result in
`health`: `{"status", "checked_at", "failures"}`. The status is `starting` until the first probe
passes, `healthy` from then on, and `unhealthy` once `retries` probes in a row have failed; one that
passes sets it back to `healthy` and clears the count. `checked_at` is the last probe. Both are
absent on a sandbox that has no probe. `shard ls` shows the status in its `HEALTH` column, with the
count beside it while it is above zero, as `unhealthy 3/3`; each change of status is one line in
the daemon log, with the reason for a failure.

The task ticks every second and starts each due probe on its own, so a slow probe holds back no
other sandbox. A sandbox's next probe waits for its last one to end, so it runs at most one timeout
late. A command probe that outruns its timeout is ended inside the
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

The policy is fixed at create. `shard-init` gets it as flags in the bundle and has no control channel,
so nothing changes it on a sandbox that runs. `shard-init` counts every start again in a file, and the
`restart-policy` task reads that file every second for every running sandbox that has a policy and
copies it onto the record, so the record is at most a second behind, and a `stop` reads the count once
more before it writes the stopped record. On gVisor, runc and Sysbox that file sits under `/.shard`,
where the guest can write it, so the daemon reads only a regular file of at most 4 KiB and refuses a
symbolic link, a fifo or a device. The record carries `restart`: `{"policy", "retries", "backoff",
"count", "last_at", "gave_up"}`, absent on a sandbox without a policy, and `retries` is omitted when
the count is unlimited. `shard ls` shows it in the `RESTART` column beside the OOM policy, as
`on-failure 2` when unlimited, `on-failure 2/5` under a cap, then `on-failure 5/5 gave up`, and
`always 7`; each start again and the give-up are one line in the daemon log. The count is for one run
and `shard-init` never clears it: a `start` of a stopped sandbox begins a new run at zero, a `resume`
and a `fork` keep the count with the process, and a `clone` inherits the policy alone. This is a
convenience for a flaky entrypoint, not a lifetime mechanism: `stop` stays the only thing that ends a
sandbox.

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
curl --unix-socket /var/lib/shard/shard.sock -T ./app.conf 'http://localhost/v0/sandboxes/<id or name>/files?path=/srv/app.conf&mode=600'
curl --unix-socket /var/lib/shard/shard.sock -o app.conf 'http://localhost/v0/sandboxes/<id or name>/files?path=/srv/app.conf'
curl --unix-socket /var/lib/shard/shard.sock -I 'http://localhost/v0/sandboxes/<id or name>/files?path=/srv/app.conf'
curl --unix-socket /var/lib/shard/shard.sock 'http://localhost/v0/sandboxes/<id or name>/ls?path=/srv'
curl --unix-socket /var/lib/shard/shard.sock -X POST -d '{"path":"/srv/cache","mode":"700","parents":true}' http://localhost/v0/sandboxes/<id or name>/mkdir
curl --unix-socket /var/lib/shard/shard.sock -X DELETE 'http://localhost/v0/sandboxes/<id or name>/files?path=/srv/cache&recursive=true'
tar -cf - app | curl --unix-socket /var/lib/shard/shard.sock -T - 'http://localhost/v0/sandboxes/<id or name>/archive?path=/srv'
curl --unix-socket /var/lib/shard/shard.sock 'http://localhost/v0/sandboxes/<id or name>/archive?path=/srv/app' | tar -xf -
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
  and says what is missing. `shard info` asks the host and the root instead of the socket, so it
  answers before a daemon exists and says what one started now would run, which is not what this
  route says when the daemon that is up was started with other flags. `docs/provider.md` says what
  a root and a host pick.
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
  the plain create answers at once. A wait that sends `Accept: application/x-ndjson` streams the
  pull instead: one `{"event"}` line per step as it lands, then `{"sandbox"}` with the settled
  record. 400 when the body does not decode or a field does not validate, or
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
  when the sandbox is not running, or when the provider does not claim the verb. A client that hangs
  up does not cut the pause, and the daemon gives a pause 10 minutes at most (`DefaultPauseBudget`)
  before it cuts it. A pause the substrate lost after its checkpoint began, a cut one included,
  answers 500 and leaves the record `failed` with the reason.
- `POST /v0/sandboxes/{id}/resume` takes no body and answers 200 with the running record. 404; 409
  when the sandbox is not paused, when its record names no snapshot, or for an unclaimed verb.
- `POST /v0/sandboxes/{id}/fork` takes `{"name"}` and answers 201 with the new record, run from the
  source's snapshot. 400 for a body that does not decode or a name that does not validate; 404; 409
  when the source has no snapshot, or for an unclaimed verb.
- `POST /v0/sandboxes/{id}/clone` takes `{"name"}` and answers 201 with the new record, run from a
  copy of the source's files. 400 as fork; 404; 409 when the source is still up.

- `POST /v0/sandboxes/{id}/exec` takes `{"command", "env", "workdir", "user", "stdin", "tty",
  "size": {"rows", "cols"}, "attach"}`, where `attach` holds the output for the first attach under the
  same 30 s bound, validates it, starts the command at once and answers 201 with the exec
  record: `{"exec", "sandbox", "command", "state": "running"|"exited", "exit_status": {"code",
  "signal"} or null, "started_at", "exited_at", "truncated", "lost_bytes"}`. 400 for a body that does not decode or
  a request that names no command; 404; 409 when no command can run in the sandbox.
- `GET /v0/sandboxes/{id}/exec` answers `{"execs": [...], "next"}` with every exec the sandbox holds.
- `GET /v0/sandboxes/{id}/exec/{exec-id}` answers the exec record. With `?wait=true` it holds the
  answer until the command ends, then answers the ended record. With the WebSocket handshake it
  answers 101 instead and attaches: it replays the buffered output, then streams live to the exit on
  stream 3. 404 when the exec ended with the sandbox, was evicted by the 32-exec cap, or belongs to
  another; 409 `in_use` for a second attach. A drop leaves the command running, so a later attach
  replays it again. A stalled client is closed with no exit, and the record says how much it lost.
- `POST /v0/sandboxes/{id}/exec/{exec-id}/kill` takes `{"signal": "TERM"|"KILL"}`, the default being
  TERM, signals the running command and answers 204. 404; 409 `exec_exited` once the command ended.
  An exec that a signal ends reports code 128+n and signal 0 on every provider. Only the entrypoint's exit status carries the signal.
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
- `GET /v0/sandboxes/{id}/egress-log` answers 200 with the newest 10000 egress decisions of the
  sandbox as a JSON array, oldest first: the proxy's own records and the host drops the daemon wrote
  into the same file. The `Shard-Egress-Cut` header counts the older records it left out, and is
  absent when it left out none. 404. `shard logs --egress` prints one record per line.
- `GET /v0/sandboxes/{id}/egress-log?follow=true` with the handshake is text messages, one JSON record
  each, live. A stopped or removed sandbox ends it with close 1000 and the reason as the close text; a
  failure of the follow is close 1011 with the error. Without the handshake it is 200 chunked
  `application/x-ndjson`, one record per line as it lands, and the body ends on the same stop or rm.
  404 either way, before anything is on the wire.
- `PUT /v0/sandboxes/{id}/files?path=&mode=&user=&parents=` streams the body into the running guest
  and answers 204 once it sits at `path` as one file: the guest writes a temp name beside it, syncs
  it and renames it over the old one, so a put that dies midway leaves the old file whole. `mode` is
  the octal permission bits, 0644 by default and at most 0777. `user` resolves as an exec's does, the
  entrypoint user by default; the write runs as that user, who owns the file. `parents=true` creates
  the missing directories. The body needs a `Content-Length`. 400 for a relative path, a chunked
  body, a bad mode or a write the guest refuses; 404 for no sandbox or a missing directory; 409 when
  the sandbox is not running.
- `GET /v0/sandboxes/{id}/files?path=` answers 200 `application/octet-stream` with the file and its
  `X-Shard-Stat`, chunked to the end of the file and never cut at the stat's size, which a `/proc`
  file states as 0. A guest that fails after the 200 cuts the chunked body before its last chunk, so
  the client reads an unexpected EOF. 400 for a directory or anything else not a regular file; 404;
  409 as above.
- `HEAD /v0/sandboxes/{id}/files?path=` answers 200 with no body and `X-Shard-Stat:
  {"type", "size", "mode", "uid", "gid", "mtime"}`, `type` one of `file`, `dir`, `symlink` or
  `other`. It never follows a final symlink. A refusal has the status alone. `shard cp` speaks all
  three.
- `GET /v0/sandboxes/{id}/ls?path=` answers 200 `{"entries": [{"name", "type", "size", "mode", "uid",
  "gid", "mtime"}]}`, sorted by name, each entry its own lstat. It follows a final symlink to the
  directory. A guest that fails after the 200 leaves the closing `]}` off, so a cut listing never
  parses. 400 for a path that is not a directory; 404; 409 as above.
- `POST /v0/sandboxes/{id}/mkdir` takes `{"path", "mode", "parents", "user"}` and answers 204. `mode`
  is an octal string, `"755"` by default and at most `"777"`, set past the umask. `parents: true` is
  `mkdir -p`: it makes what leads to the path and takes a directory already there. `user` is as a
  put's. 400 for a bad mode, or a path already there unless `parents` is set and it is a directory;
  404 for a missing parent; 409 as above.
- `DELETE /v0/sandboxes/{id}/files?path=&recursive=` removes the path and answers 204. A symlink goes,
  never its target. A directory with anything in it needs `recursive=true`, and `/` is never
  deleted. It runs as the entrypoint user. 400 for `/` or a full directory without `recursive`; 404;
  409 as above.
- `PUT /v0/sandboxes/{id}/archive?path=&user=` unpacks the tar body, chunked or sized, under the
  guest directory `path` and answers 204 once every entry is on disk. `user` is as a put's, and the
  unpack runs as that user, who owns every entry. Each file lands through a temp name. The guest
  refuses an absolute name, a `..`, an entry under a symlink that leaves `path`, a hard link to a
  file the tar did not carry, a device node and a fifo. Directories take their modes once their
  entries are in, and one already there keeps its own. The body is held to the same idle bound as a
  put's. 400 for a refused entry, a body that is not a tar or a `path` that is not a directory; 404;
  409 as above.
- `GET /v0/sandboxes/{id}/archive?path=` answers 200 `application/x-tar` with the path and its
  `X-Shard-Stat`, the top entry named after the path's base name and a directory's entries in
  lexical order. A guest that fails after the 200 aborts the chunked body, so a cut tar never reads
  as a whole one. 400 for `/`; 404; 409 as above. `shard cp` copies a directory through both. A copy
  out unpacks what the sandbox sends, so it refuses an entry or a symlink that leaves the
  destination, a device node and a hard link to a file the tar did not carry, drops setuid and
  setgid, and stops past 64 GiB or 1048576 entries.
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
  reference that does not parse; 500 when the registry or the unpack failed. With `Accept:
  application/x-ndjson` it streams one `{"event"}` line per step, then `{"image"}`.

**A streamed pull says each step as it lands.** An event is `cached` (the image is already on disk),
`pulling` (the reference, the digest, the layer count and their bytes), one `layer` per layer with
its bytes and whether it was already on disk, `unpacking` with the layer count and one `unpacked`
per layer in manifest order with its position when the tree is not on disk yet, a `building` with
the path of each disk or EROFS image a VM provider boots from, then `pulled` with where the image
went. A refusal before the first line keeps its status and its JSON body. After the first line the
status is sent, so a failure is a last `{"error"}` line with the same `code` and `message`.
- `DELETE /v0/images/{ref}` takes the whole reference, slashes and all, and answers 200 with a
  `warnings` array of what it could not delete under the store. 404; 409 naming every sandbox that
  references it, unless `?force=true`.
- `POST /v0/images/prune` removes every image no sandbox references and answers
  `{"removed": [...], "warnings": [...]}`. It refuses with 500 rather than guess when a record is
  unreadable, because an image a sandbox needs would be gone.

A stream is a WebSocket (RFC 6455) on the same route, opened with the standard handshake. Every
refusal comes before the 101 as a status and a JSON body. An exec carries binary messages whose
first byte is the stream and the rest the payload: the client sends 0 (stdin) and 4 (stdin closed,
empty); the daemon sends 1 (stdout), 2 (stderr), 3 (exit, `{"code", "signal"}`, plus `"lost_bytes"` when
output was lost and `"error"` when the command never ran) and 5 (a failure of the daemon's own, `{"error": {"code", "message"}}`, the
same object every error body carries). One payload is at most 1 MiB, and a longer write goes as several messages. 3 or 5 ends the
session and the daemon closes with 1000; a client that closes first leaves the command running, to re-attach by its exec id. A `tty` exec
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
| `invalid_request` | 400 | the body does not decode, a field does not validate, or a named secret, policy or image is unknown. Also the TCP front, when the request line does not parse as net/http parses it; nothing is dialed |
| `body_too_large` | 413 | a JSON body over 1 MiB; the daemon reads no further and closes the connection after the answer |
| `not_found` | 404 | no sandbox, policy, secret, image or exec has the reference, or no route has the path |
| `sandbox_not_running` | 409 | exec or pause on a sandbox that is not running, or one the substrate no longer holds |
| `sandbox_not_stopped` | 409 | start, clone, or rm without force on a sandbox that is up, and rm without force on a paused one, whose snapshot a resume needs |
| `sandbox_not_paused` | 409 | resume or fork on a sandbox that is not paused |
| `sandbox_failed` | 409 | any verb but a get or an `rm` on a create that ended `failed`; the message carries the `failed_reason`, and `rm` frees it |
| `sandbox_live` | 409 | grant, ungrant, attach or detach while the sandbox runs or is paused |
| `no_snapshot` | 409 | resume or fork on a paused sandbox whose record names no snapshot |
| `unsupported` | 409 | the provider does not claim the verb |
| `in_use` | 409 | delete a policy, secret or image that sandboxes hold, or move the placeholder of a secret they hold; `error` adds `"holders": [ids]`. Also a second attach of an exec, with no holders |
| `name_taken` | 409 | a create whose `name` another sandbox already holds |
| `unauthorized` | 401 | the TCP front, when the request carries no valid bearer token; nothing is dialed |
| `forbidden` | 403 | the TCP front, when the token is valid but its scopes do not reach the route; nothing is dialed. Also the daemon, on a create that names a secret without `secret:*` or a policy without `policy:*` |
| `substrate_timeout` | 504 | a stop, rm or restart whose substrate status call did not answer within the budget; retry it once the runtime frees. On gVisor, rm --force reclaims through the wedge instead: it SIGKILLs the sandbox's own runsc processes, matched by its cgroup and by its id on their command line, then finishes the teardown, and answers this code only when that kill fails too |
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
after it, pass through untouched and every route above works unchanged. The front splits the request
line on the ASCII space alone and checks the method and the version as net/http does, so it reads the
route the daemon serves; a line that does not parse that way, such as one split by a non-breaking
space, is a `400` with the code `invalid_request`, written before the token check and before anything
is dialed. A bad or missing token is a
`401` with the code `unauthorized`, written before anything is dialed, so an unauthenticated client
never reaches the daemon. A socket that does not answer is a `502` with the code `internal`. The
front verifies HS256 alone: a token signed by another algorithm, a token signed by another secret, a
token with no subject, a token with no id, an expired token, a token whose id the ledger does not
hold, and a revoked token are each the same `401`. A token with no `exp` never expires; the front
enforces `exp` only when the token carries one. Neither
the front nor the CLI ever logs a token or the secret, and the front logs the subject of every
request it lets through. Without `--cert` and `--key` the front refuses to start: there is no plain
TCP mode to fall back to. The secret file must not be readable by everyone on the host, and the front
refuses one that is. The secret must be at least 32 bytes, the width an HS256 key needs, and the front
refuses a shorter one; `openssl rand -hex 32` prints a secret that passes.

A connection is bounded before its token is checked. The front holds at most 32 connections that
have not shown a valid token yet from one source address, and at most (soft `RLIMIT_NOFILE` - 64) / 2
in total, read at start and never fewer than 32: a held connection costs two file descriptors once it
dials the daemon socket, and 64 stay for the listener, the logs and the dials. It closes the next one
at once and logs the refusal, a few lines a second per source at most. A connection leaves that count
once its token passes, so a client that holds many `logs` follows or exec sessions open is never
refused for them. A connection must send its whole request head, the TLS handshake included, within
10 s, or the front closes it. An accept that runs out of file descriptors or memory waits from 5 ms up
to 1 s and tries again, so a flood of connections never ends the front.

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
| `exec` | every `exec` route, and every `files`, `ls`, `mkdir` and `archive` route |
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

The route check is coarse: `create` maps to `sandbox:write` alone, yet a create can name secrets and
a policy, which `secret:*` and `policy:*` otherwise guard. So the daemon checks the token's scopes a
second time: a create that names a secret needs `secret:*`, and a create that names a policy needs
`policy:*`, or the daemon answers `403` with the code `forbidden` and names the missing scope before
it builds anything. The front carries the token's scopes to the daemon in an `X-Shard-Scopes` header,
stamped on every request it forwards and stripped of any copy the client sent, so a forged header can
only remove a right. A request with no such header reached the daemon socket directly, which is the
operator's own channel and keeps every right. A `fork` and a `clone` keep the source sandbox's grants
by design, so they need only `sandbox:write`.

A token is minted on the server, from the same secret, and never over the API:

```
shard tokens mint --name ci --duration 24h --secret-file /etc/shard/serve.secret
shard tokens mint --name reader --scopes sandbox:read,exec --secret-file /etc/shard/serve.secret
```

`mint` prints one JSON object to stdout and exits: the token, its `expires_at` (RFC 3339 in UTC,
equal to the token's `exp`, and `null` when the token never expires), and the `scopes` it carries.

```
{"token":"<jwt>","expires_at":"2026-09-18T15:40:00Z","scopes":["*"]}
```

It is a local verb like `daemon` and `serve`: it never reaches the daemon, and the daemon never sees
the secret. `--name` is the subject the front logs, `--duration` defaults to 0, which mints a token
with no `exp` that never expires, and `--scopes` is a
comma-separated list of the scopes the token carries; an empty `--scopes` mints `["*"]`, every verb,
so pass `--scopes` for any token but an operator's. A scope that is neither `*` nor one of the eight
capabilities above is refused, the error lists them, and nothing is recorded. The client's
`--token-file` takes this object whole or the bare token, so `shard tokens mint ... >
ci.token` needs no extra step. Rotate the secret and every token it signed stops verifying at once.

The front reads the secret file once, at start, so a rotation needs a `shard serve` restart, and that
restart ends no connection that is already spliced.

### Tokens

Every minted token carries a random 128-bit `jti`, and `mint` appends one record for it to a ledger:
the id, the subject, when it was issued, when it expires, its scopes, and whether it is revoked. The
ledger sits beside the secret file, at `serve.tokens` in the same directory, and `--tokens-file`
overrides that path on `tokens mint`, `tokens ls`, `tokens revoke` and `serve`. `mint` creates it `0640` when it is
absent, refuses one that everyone can read, and prints no token when it cannot write the record.

```
shard tokens ls --secret-file /etc/shard/serve.secret
shard tokens revoke --secret-file /etc/shard/serve.secret <id>
shard tokens revoke --name ci --secret-file /etc/shard/serve.secret
```

`tokens ls` lists every record with the status a request would see now: `active`, `revoked` or
`expired`. `revoke` marks one token by its id, or every token of a subject with `--name`, so the next
request that carries it is a `401`. Both are local verbs, like `mint`: they never reach the daemon.

The front reloads the ledger when its size or its modification time changes, so a `revoke` takes
effect on the next request with no restart. A ledger the front cannot read at start stops it from
starting, and a ledger that vanishes while the front runs turns every request into a `401`.

`mint` and `revoke` take an advisory lock on the ledger, at `serve.tokens.lock` beside it, so
parallel revokes and a mint that races a revoke never lose a record. The front only reads, so it
takes no lock.

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
nothing else is contended between processes. The lock dies with the process. Beside it the daemon
writes `daemon.pid`, for newsyslog to signal on a Mac, and removes it on a clean exit; nothing in
shard reads it. Neither file is probed to decide whether a daemon is up: a client that needs one
asks the socket and reads the outcome.

## Supervision

Each piece of background work is a task. A task that returns an error is restarted with exponential
backoff and jitter, capped at a minute; a run that stays up long enough earns a fresh backoff, so a
crash loop backs off and a one-off crash restarts fast. A panic in a task is contained and counts as
a failure. A task that returns nil is done and stays done until the daemon restarts. The `api` and
`proxy` tasks return an error when a listener dies, so they are restarted, and nil only when the
daemon is ending.
