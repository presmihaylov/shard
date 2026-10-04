# The daemon

`shard daemon` is the resident process that owns the state. The CLI is a thin client of it. Every
verb goes over the socket, and no verb reads or writes the state itself. Without a daemon, each
verb fails fast with one line:

```
shard: cannot connect to shard daemon at /var/lib/shard/shard.sock: is it running? systemctl status shard
```

On a Mac the unit is the LaunchDaemon, so the hint there is `launchctl print system/shard.daemon`.
The unit serves `/var/lib/shard` only. Under any other `--root`, the hint names that root's own
daemon instead (`is it running? shard --root /srv/shard-e2e daemon`). Under `--remote`, it names
the proxy and the front behind it (`the proxy at shard.example.com and the shard serve behind it`).

`shard daemon` itself is the one exception, because it is the daemon process rather than a client
of one. No verb starts the daemon. A resident root process is installed on purpose, through the
systemd unit in `packaging/systemd/shard.service`. A release carries the unit beside the two Linux
binaries, so an install needs no checkout. As root:

```
base=https://github.com/presmihaylov/shard/releases/latest/download
for f in shard-linux-amd64 shard-init-linux-amd64 shard.service SHA256SUMS; do curl -fsSLO "$base/$f"; done
sha256sum --ignore-missing -c SHA256SUMS
install -m0755 shard-linux-amd64 /usr/local/bin/shard
install -m0755 shard-init-linux-amd64 /usr/local/bin/shard-init
install -m0644 shard.service /etc/systemd/system/shard.service
systemctl daemon-reload
systemctl enable --now shard
```

The daemon looks for the guest supervisor at `/usr/local/bin/shard-init`, and `SHARD_INIT_PATH`
names another path. Install the provider's runtime first: `runsc`, `sysbox-runc` or `runc`
(`docs/provider.md`). From a checkout, `make build-linux build-shard-init-linux` builds the same two
binaries into `bin/`, and the unit is the file in `packaging/systemd`.

On a Mac the equivalent is the LaunchDaemon in `packaging/launchd`, which `docs/mac.md` explains
how to install.

## What the daemon owns

- The API socket under `${root}/shard.sock` serves the REST surface. The section "The API socket"
  below describes it.
- The egress proxy is the `proxy` task. It listens on the bridge gateway, on ports 30080 and 30443,
  and the web traffic of every fronted sandbox goes through it. The daemon restarts it after a
  crash, like any task.
- Output log rotation keeps a sandbox's `output.log` and a VM's `console.log` at 16 MiB each.
  Beside each one is one older file of up to 16 MiB, `<file>.1`, which `shard logs` prints first.
  The daemon writes a VM's `output.log` itself and renames it before it passes 16 MiB, so that
  bound is exact. runsc and runc hold their own `output.log`, and the VMM holds `console.log`. For
  those files the `held-log-rotation` task copies the last 16 MiB of each to `.1` and truncates the
  file in place, every second and at once on a start. That bound is soft by one second of output.
  Nothing bounds a log while the daemon is down, and anything a sandbox writes between the copy and
  the truncate is lost.
- The liveness loop is the `liveness` task. Every 5 s it asks the substrate about every record that
  says `running`, and makes the record agree. It records an entrypoint that exited, stops a sandbox
  whose process is gone, and brings back a sandbox that the host ended for its memory. The section
  "Liveness" below has the details.
- The restart policy is applied by `shard-init`, which starts the entrypoint again under the policy
  the sandbox was created with. The `restart-policy` task copies the restart count onto the record
  every second. The section "Restart policy" below has the details.

- The stores hold the images under `${root}/images`, the policies, the secrets and the sandbox
  records. One writer owns them, so they need no lock between processes. The daemon serializes its
  own writes in memory, and every client goes through it. The value of a secret crosses the socket
  once, on the `PUT`. The daemon writes it only to the secret store, and never logs it or lists it
  back. A create writes its sandbox record before it pulls, and a cached pull waits for any removal
  in flight. `image remove` and `image prune` both free images by reachability over the records and
  the snapshots, so neither of them sweeps a rootfs in the middle of a create.
- The sandbox lifecycle verbs `create`, `start`, `stop`, `remove`, `exec`, `logs`, `pause`, `resume`
  and `fork`, and the snapshot verbs, run inside the daemon, in `services/sandbox`. The image pull of
  a create runs there too. The client waits for the pull with no deadline, and the daemon does not
  stream the pull's progress back to the client yet. The daemon serializes the verbs on one sandbox
  with an in-process mutex per id. A stop and a remove on the same sandbox therefore run one after
  the other, while two verbs on different sandboxes run side by side.

The daemon holds the guest side of an exec, which is its pipes and, for a `-t` exec, its pseudo
terminal. The CLI keeps only the local terminal: raw mode, and the `SIGWINCH` it forwards.

A sandbox outlives the daemon. runsc runs in its own session, so when the daemon stops or restarts,
every sandbox stays up, and the next daemon lists and serves them from the same root. For the same
reason the unit sets `KillMode=process`, so that systemd ends only the daemon and leaves its
sandboxes running.

An exec is a resource the daemon owns for the life of the sandbox. The daemon starts the command at
once and keeps the last 8 MiB of its output. The create answers only once the command's `execve`
took, so a command that cannot start gets no exec id. A client that drops can re-attach by exec id, replay
what it missed and stream the rest. One client attaches at a time. The command waits for that client
rather than evict output the client has not taken. A client that takes nothing for 30 s
(`ExecStallBound`) is detached, the command keeps running, and `lost_bytes` counts what no client
read. Once the command ends, the record holds its exit. While the command runs, `kill` signals it.
Only a `DELETE` of the exec or a `stop` of the sandbox frees it. The daemon keeps at most 32 exited
execs per sandbox, so a new exec evicts the oldest exited one and the retained output stays bounded.
A running exec never counts toward that limit. An evicted exec answers 404, the same as a deleted
one.

An exec does not outlive the daemon. The daemon holds the record and the buffer in memory, while
`shard-init` holds the guest process. A restart therefore cuts off every client and loses the
record, and the command keeps running inside the sandbox. The `runsc exec` driver dies with the
daemon. Before it serves, the next daemon sweeps the scratch that every exec keeps under
`<root>/exec`. A new `shard exec` answers as soon as the daemon is back.

## The data dir on Firecracker

Firecracker copies a disk where gVisor copies an overlay layer. A `fork` or a snapshot on
Firecracker is therefore fast only on a filesystem that clones a file by sharing its blocks, which
means XFS with `reflink=1`, or Btrfs. Before it takes the lock, a daemon with
`--provider firecracker` probes its root with one real clone. The daemon leaves a root that clones
alone. A root that does not clone (ext4, on most hosts) gets a loopback XFS image beside it at
`<root>.xfs`. The daemon formats that image with `mkfs.xfs -m reflink=1`, mounts it over the root
with `-o loop`, and adds a line to `/etc/fstab` so that it comes back on boot. The image takes half
the free space of the filesystem that holds it, up to 100 GiB, and its size stays fixed once it
exists. Every step skips what is already done, so a restart is a no-op.

In some cases the daemon refuses instead of provisioning, and says why and what to do. It refuses
when it is not root, when `mkfs.xfs` (xfsprogs) is missing, when the root is already a mount that
cannot clone, when the root holds entries the mount would hide, when a file at `<root>.xfs` is not
an XFS image, or when half the free space is under 10 GiB. It never falls back to a full copy. No
other provider provisions or probes anything (SHARD-264).

To undo the bootstrap by hand, first stop every sandbox with `shard stop`. Then stop the daemon
(`systemctl stop shard`) and keep it stopped. The order matters because a sandbox outlives the
daemon, and a live VM or daemon holds files under the mount. An unmount then fails as busy, and a
lazy one detaches live state. Next, unmount the image from the root, remove its `/etc/fstab` line,
and delete `<root>.xfs`, `<root>.xfs.lock` and the root. The daemon writes the line as
`<root>.xfs <root> xfs loop,nofail 0 0`. It escapes each path the way `getmntent` reads it: `\134`
for a backslash, `\040` for a space, `\011` for a tab and `\012` for a newline. A grep for a root
with a space in it must therefore search for `\040`. Remove the fstab line before the next boot, or
the host mounts the image again.

## Reconcile at start

The daemon checks every record against the substrate after it takes the lock and before it listens,
so no verb ever reads a record that the substrate disagrees with. It corrects a record but never
deletes one. It handles these cases:

- A record that says `running` with no process becomes `stopped`, and its `stopped_reason` says
  `daemon restarted and found no process`. `shard list --all` prints the reason beside the state, and
  `shard inspect` carries it in the record. A `start` clears it. A vz shim that is there but does
  not answer within 5 s makes the record `unresponsive` instead, and so does a firecracker vmm that
  does not answer within 4 s. The record keeps that process's pid, and the daemon never kills the
  process for its silence.
- Sometimes the host ended a sandbox for its memory while the daemon was down, and its record still
  says `running`. That record gets the same decision the liveness tick makes for an OOM the daemon
  saw: it becomes `stopped` with `ran out of memory and the host ended it`, and nothing starts it
  again. No verb therefore reads the record as `running` with no process behind it (SHARD-311).
- A record that says `paused` keeps its state while its checkpoint is on disk. A paused sandbox
  has a checkpoint instead of a process, and `resume` still brings it back from that checkpoint. A
  paused record whose checkpoint is gone becomes `stopped` with the same reason. Only an
  absent checkpoint counts as gone. A read that fails for any other reason leaves the record
  `paused`, so one bad boot cannot end every future `resume` while the checkpoint sits on disk.
- A record that says `running` becomes `paused` when it has a `pause` in flight, no process behind
  it, and a complete checkpoint in its checkpoint directory. In that case the daemon stopped after
  the pause installed the checkpoint and before the pause wrote the record. A pause removes the old
  checkpoint before it marks the record, so a checkpoint found under the mark belongs to that pause.
  Without the mark, the checkpoint is one that an earlier pause left, and the record becomes
  `stopped` as above. The liveness tick applies the same rule. On gVisor the sentry can still be
  frozen beside that checkpoint, so the daemon deletes the frozen sandbox first, without a thaw, as
  the pause would have done. If the daemon stopped after that delete, runsc holds nothing, but the
  rootfs is still mounted. The daemon unmounts the rootfs once the sandbox's cgroup is empty, so
  `remove --force` still frees the record. On Firecracker the vmm can still be paused beside that
  checkpoint, and the next daemon ends it the same way. A paused vmm beside a complete checkpoint in
  the sandbox's checkpoint directory is never resumed past that checkpoint. A substrate that cannot
  release a frozen sandbox keeps the record as it is. A marked record that the substrate says is
  `running` stays `running` and loses the mark. That pause is over and never took this run, so a
  later death of the run is a stop, not a pause. On vz this is a pause cut after its checkpoint, and
  the next daemon runs on its shim. Any other live state, frozen or unresponsive, proves no such run
  and keeps the mark. The liveness tick asks the substrate again under the sandbox's lock before it
  drops a mark, because a pause can commit after the tick's first probe.
- A record that says `stopped` while the substrate holds a live process becomes `running`, with the
  pid the substrate reports. The daemon drops the exit status of the run that ended.
- A gVisor source that a live fork marked when the daemon was cut carries the mark beside it, frozen
  or already running again. The first read of it, at startup or on a liveness tick, thaws a frozen
  source and drops the mark only once the source runs again. A stale mark on a running source would
  pass a later real pause for a cut fork, so the read drops it there too (SHARD-457).
- A Firecracker source that a live fork's capture paused when the daemon was cut carries a marker
  beside it. The first read of it resumes and thaws the source, then drops the marker. It never ends
  that source as it ends a pause cut after its install (SHARD-462).
- A record that says `created` becomes `failed`, and its `failed_reason` says
  `the daemon restarted before the fork finished`. No verb leaves a record in `created`. Only the
  copy of a fork passes through that state, and the caller got an error instead of the id. The
  daemon first stops a copy whose process still runs, because `remove` refuses a live sandbox and
  `stop` refuses a failed one. Then it tears down the copy's substrate, as `remove` does, because a restore
  that the old daemon started can keep running where the runtime cannot see it. On gVisor that
  teardown first kills any `runsc restore` of the copy. Until that restore starts the sandbox, it
  runs outside the sandbox's cgroup, and the daemon lock means no new restore can start. A fork or
  resume records the binary and the command line of its restore in `restore.json` before it runs, so
  the kill finds the restore even after runsc was replaced. The kill signals through a pidfd, so it
  never hits a pid that was reused in between. A copy with no `restore.json` comes from a daemon
  older than the file. For such a copy, the daemon matches a process that the daemon's user started
  with the restore command line on the copy's own bundle, from any checkpoint and any runsc binary.
  A teardown that fails leaves the record as it is, with a line in the log, and the next start of
  the daemon tries again.
- A record that says `pending` becomes `running` when the substrate holds its process, because a
  start that took effect before the daemon stopped did reach `running`. With no process behind it,
  the record becomes `failed`, and its `failed_reason` says
  `the daemon restarted before the create finished`. A create that the daemon was still running
  when it stopped never reached `running`, so the record is terminal and only `remove` frees it.
- Host netfilter is the policy of record, and nothing re-applied it while the daemon was down. So
  when any sandbox runs, the whole table goes back on, once.

Each corrected record is one line in the journal. Sometimes the daemon cannot check a record,
because the substrate refused the probe or a write failed on a full root. The daemon then logs one
line that names the sandbox and the error, and leaves the record as it is. It does not refuse to
start, because a daemon that refused could not serve the other sandboxes, nor `remove` the one that
fills the root (SHARD-341). A daemon that cannot list the records or put the host rules back does
refuse to start, and systemd restarts it. A record the daemon cannot read is named in the log and
left as it is, because shard cannot correct a record it cannot read. The vz and Firecracker
providers write the initrd and a replayed restart count only when the bytes on disk differ, so a
start on a full root writes nothing it already has. A root with no records needs no substrate, so a
host without `runsc` still gets a daemon that answers the reads and the store verbs.

## A full root

A daemon that cannot start on a full root cannot serve the `remove` that would free it. The daemon
therefore holds 64 MiB back in `<root>/.reserve` (SHARD-351). It writes the file under the lock, as
real blocks, and only when the root has at least four times that much free. On a fuller root the
daemon starts without a reserve and logs a line.

Every write that a start makes gets one retry. This covers the reflink probe of the data dir, the
initrd of the vz and Firecracker providers, the reconcile record writes and the socket bind. When a
step fails with `ENOSPC`, the daemon deletes `.reserve` and runs the step once more, at once. It logs
one line with the step and the free space before and after. A second `ENOSPC` fails the step. The
next start that has room writes the reserve again.

Known limit: on macOS, a Time Machine local snapshot of the Data volume can keep the blocks of a
deleted file, so the delete gives back less than the reserve. The log line says how much less.

## Liveness

Every 5 s the `liveness` task asks the substrate about every record that says `running`, and makes
the record agree with it. It asks under a 10 s deadline and outside the per-sandbox lock. A
substrate call that wedges therefore never pins that lock, and a `stop` or `remove` on that sandbox, or
on any other, still runs. When the substrate does not answer within the deadline, the tick logs one
line, leaves the record untouched, and asks again on the next tick. The tick takes the lock only to
write, and it reads the record again first. A `stop` that landed since the list therefore wins, and
the tick leaves that sandbox alone. A tick that the substrate answers has four possible outcomes.

The entrypoint exited but the sandbox is still up. The sandbox outlives its entrypoint, so the state
stays `running`, and the tick writes the exit into `exit_status`. `shard list` then shows `running
(exited 7)`, and `shard inspect` shows the code and the signal. An operator can therefore tell a
clean exit from a crash without a `stop`. The exit comes from the file that `shard-init` writes, the
same file a `stop` reads (SHARD-168 replaces the channel underneath but keeps the value). A `stop`
that follows uses the recorded exit and does not wait for it again.

The sandbox process is gone. Only the host or a crash ends a running sandbox behind the daemon's
back, so the tick makes the record `stopped` with `pid` 0 and `stopped_reason` `the sandbox
process died`. A `start` brings it back. The daemon does not start it again on its own. A restart
policy for a process that died is SHARD-188.

`shard-init` itself died. On Firecracker, `shard-init` tells the host why before the VM halts, with
the exit code 125, as `docs/provider-vz.md` describes. The tick makes the record `stopped` with
`pid` 0, `exit_status` 125 and `stopped_reason` `shard-init failed: <reason>`, and logs one line. A
`start` brings it back and clears both. A run whose last report never landed ends the same way, with
the loss as the reason. Its `start`, however, answers with the loss until `remove` drops the sandbox.

The host ended it for its memory. When a sandbox overruns its `--memory` bound, the host ends the
whole sandbox. The kernel kills every process in its cgroup, `shard-init` included, and `runsc`
still holds the dead container. On Firecracker and vz the guest kernel kills it, and `shard-init`
reports the kill to the host before the VM halts. A sandbox that the host ended for its memory
becomes `stopped` with `pid` 0, and its `stopped_reason` says `ran out of memory and the host ended
it`. Until a tick acts on it, an `exec` on it answers 409 with the same words. Nothing starts it
again, as the next section says.

## Health checks

The daemon runs no probe of its own (SHARD-455). It watches the sandbox process and the
entrypoint, through the liveness task and the restart policy, and never the application inside. A
client that wants to know whether its workload is up runs the check itself with `shard exec`, on
its own schedule, and acts on the exit code:

```
shard exec web sh -c 'test -e /ready'
```

`exec` exits with the command's code, so a script can stop or restart the sandbox on a failure.

## After an OOM

The daemon never starts a sandbox again after the host ended it for its memory (SHARD-461). The
sandbox keeps its files, so a `shard start` brings it back over them and clears the reason. The
bound is fixed at create, so a sandbox that needs more memory is a new sandbox with a larger
`--memory`.

## Restart policy

A sandbox outlives its entrypoint, and the policy does not change that. The policy starts the
entrypoint again inside the sandbox that is already up, and the sandbox stays `running` whatever the
policy does. `shard run --restart <policy>` (`"restart": {"policy"}` in the create body) takes one of three
values. `no` is the default. `on-failure` starts the entrypoint again after an exit other than 0, or
after a signal. `always` starts it again after every exit. `--restart-retries` (`retries`) caps the
starts again in one run. With no cap, `on-failure` starts the entrypoint again without end. `always`
never gives up and takes no retries at all. `--restart-backoff` (`backoff`, in whole seconds,
default 1) is the wait before the first start again, and it doubles each time, up to 60 s. Both
flags need a policy. The three flags go on `shard run` only, and `shard create` and `shard exec`
refuse each one by name. A body with a policy other than `no` and no `command` answers 400 naming
`restart.policy`. A run that lasts ten seconds since its last start clears the count, so a
slow crash loop never spends a finite cap. At the cap, `on-failure` gives up and the entrypoint
stays exited. A stop then puts its last exit in `exit_status`, as after any exit. A stop during the
wait ends the sandbox at once and drops the start that was due.

The policy ends when no start again follows an exit: any exit under `no`, a clean exit under
`on-failure`, the give-up at the cap, or a stop of the app. `shard-init` then writes `ended`, and that
last exit is the app's. `shard run` waits for it and exits with that code. `POST app/stop` cancels
every start again and sends TERM to the app, or KILL with `force`, and the sandbox stays `running`
with `shard-init` alone. A process in the sandbox does the same with `kill -USR1 1` for TERM and
`kill -USR2 1` for KILL. A stop during the wait ends the app at once.

The app leads its own process group, and every end of a run signals that group whole: `app/stop`,
the Ctrl+C of `shard run`, a start again and an OOM kill. Once the app's own process exits,
`shard-init` kills what is left in the group before it writes `ended` or starts again, so a child
that ignores TERM ends too. So what the app forked ends with it, while `shard-init` and every exec
session run on. A process that leaves the group, with `setsid`, runs until `stop`.

The policy is fixed at create. `shard-init` gets it as flags in the bundle and has no control
channel, so nothing can change it on a running sandbox. `shard-init` counts every start again in a
file. Every second, the `restart-policy` task reads that file for every running sandbox that has a
policy, and copies the count onto the record. The record is therefore at most a second behind. A
`stop` reads the count once more before it writes the stopped record, and the wait of a run copies
it as soon as the policy ends. On gVisor, runc and Sysbox
that file sits under `/.shard`, where the guest can write it. The daemon therefore reads only a
regular file of at most 4 KiB, and refuses a symbolic link, a fifo or a device.
The record carries `restart`: `{"policy", "retries", "backoff",
"count", "last_at", "gave_up", "ended"}`, absent on a sandbox without a policy, and `retries` is omitted when
the count is unlimited. `shard list` shows it in the `RESTART` column:
`on-failure 2` when unlimited, `on-failure 2/5` under a cap, then `on-failure 5/5 gave up`, and
`always 7`. Each start again, and the give-up, is one line in the daemon log. The count is for one
run, and `shard-init` never clears it. A `start` of a stopped sandbox begins a new run at zero, and
a `resume` and a `fork` keep the count with the process. The policy helps with a flaky entrypoint.
It is not a lifetime mechanism, and `stop` stays the only thing that ends a sandbox.

## The API socket

The daemon listens on `${root}/shard.sock`, which is `/var/lib/shard/shard.sock` by default. It
never listens on TCP. A network address is the job of `shard serve`, a separate process that the
section "The TCP front" describes. The socket is `0660 root:shard` when the host has a `shard`
group, and `0600 root` otherwise. With the group, the daemon also gives the root to it at `0710`
on every start, so the group can reach the socket by its name and list or read nothing else. The
daemon logs which one at startup:

```
api listening on /var/lib/shard/shard.sock, mode 0660, group shard
```

A unix socket needs write permission to connect, so the unit sets `UMask=0077`. The socket is
therefore never world-writable between the listen and the chmod. Before it listens, the daemon
removes a stale socket file left by a daemon that was killed. It already holds the singleton lock by
then, so it never removes the socket of a live daemon. The socket goes away with the daemon.

The routes:

```
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/version
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/capabilities
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/daemon
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/sandboxes
curl --unix-socket /var/lib/shard/shard.sock 'http://localhost/v0/sandboxes?all=true'
curl --unix-socket /var/lib/shard/shard.sock 'http://localhost/v0/sandboxes?limit=20&cursor=<id>'
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/sandboxes/<id or name>
curl --unix-socket /var/lib/shard/shard.sock -X POST -d '{"image":"alpine:3.20","command":["sleep","600"]}' http://localhost/v0/sandboxes
curl --unix-socket /var/lib/shard/shard.sock -X POST http://localhost/v0/sandboxes/<id or name>/start
curl --unix-socket /var/lib/shard/shard.sock -X POST http://localhost/v0/sandboxes/<id or name>/stop
curl --unix-socket /var/lib/shard/shard.sock -X DELETE 'http://localhost/v0/sandboxes/<id or name>?force=true'
curl --unix-socket /var/lib/shard/shard.sock -X POST http://localhost/v0/sandboxes/<id or name>/pause
curl --unix-socket /var/lib/shard/shard.sock -X POST http://localhost/v0/sandboxes/<id or name>/resume
curl --unix-socket /var/lib/shard/shard.sock -X POST -d '{"name":"web-2"}' http://localhost/v0/sandboxes/<id or name>/fork
curl --unix-socket /var/lib/shard/shard.sock -X POST -d '{"sandbox":"web","name":"web-base"}' http://localhost/v0/snapshots
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/snapshots
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/snapshots/<id or name>
curl --unix-socket /var/lib/shard/shard.sock -X DELETE http://localhost/v0/snapshots/<id or name>
curl --unix-socket /var/lib/shard/shard.sock -X POST -d '{"snapshot":"web-base","name":"web-3"}' http://localhost/v0/sandboxes
curl --unix-socket /var/lib/shard/shard.sock -X POST -d '{"command":["/bin/echo","hi"]}' http://localhost/v0/sandboxes/<id or name>/exec
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/sandboxes/<id or name>/exec
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/sandboxes/<id or name>/exec/<exec-id>
curl --unix-socket /var/lib/shard/shard.sock -X POST -d '{"signal":"TERM"}' http://localhost/v0/sandboxes/<id or name>/exec/<exec-id>/kill
curl --unix-socket /var/lib/shard/shard.sock -X DELETE http://localhost/v0/sandboxes/<id or name>/exec/<exec-id>
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/sandboxes/<id or name>/attach
curl --unix-socket /var/lib/shard/shard.sock -X POST -d '{"force":true}' http://localhost/v0/sandboxes/<id or name>/app/stop
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

- Every route is public or local. A local route answers on the socket only, and `shard serve`
  refuses it with the same `403` as an unknown route. The local routes are `GET /v0/daemon` and the
  four `images` routes.
- Every route that answers a sandbox answers the record without its host side, which stays in the
  daemon's state. In the `egress` of a record, an implied DNS rule names the group `dns` in place of
  the bridge gateway, and keeps its `id`.
- No public error body, `failed_reason`, list warning or stream error frame names a host path, a
  pid or the root. Each says what its error type made public, and the daemon log keeps the whole
  cause. A record from before this answers `the sandbox failed; the daemon log has the cause`.
- `GET /v0/version` answers `{"version": "...", "api_version": "v0"}`. `shard version` prints the
  `version` as its `daemon` line, under the `client` line of the binary that asked. `shard
  --version` prints only the `client` line, touches no socket, and never fails, like `docker
  --version`.
- `GET /v0/capabilities` answers the eight lifecycle verbs and whether this server supports each:
  `{"create": true, "start": true, "stop": true, "remove": true, "pause": true, "resume": true,
  "fork": false, "snapshot": true}`. Every provider runs `create`, `start`, `stop`, `remove` and
  `snapshot`, the copy of a stopped sandbox's files; `pause`, `resume` and `fork` are the
  provider's own. The scopes of the token and the state of any sandbox never change the answer.
  `shard capabilities` prints it.
- `GET /v0/scopes` answers `{"scopes": [{"name": "...", "description": "..."}]}`: every scope a
  token can carry, from the one table `tokens mint` checks against. It lists the valid scopes, not
  the scopes of the token that asked. `shard tokens scopes` prints it, and follows `--remote`.
- `GET /v0/daemon` answers what the daemon knows about itself: `version`, `pid`, `started_at`,
  `socket`, `provider`, `capabilities` as the provider's three booleans (`pause`, `resume`, `fork`),
  and `proxy` with `plain_port` and `tls_port`. `shard daemon status` prints it, one field per line.
  The daemon builds the provider on the first ask, so on a host without its runtime the route
  answers 500 and says what is missing. `shard info` asks the host and the root instead of the
  socket. It therefore answers before a daemon exists, and says what a daemon started now with no
  `--provider` would run. That can differ from what this route says, when the running daemon was
  started with `--provider`.
  `docs/provider.md` explains what a root and a host pick.
- Every list answers `{"<plural>": [...], "next": null | "<cursor>"}`, plus `warnings` where the
  route says so. `?limit=N` caps the page. `?cursor=<c>` serves the items whose key sorts after the
  cursor, in the order of the list. The `next` of the previous page is the key of its last item,
  which is the id of a sandbox or a snapshot, the name of a policy or a secret, or the reference of
  an image. A
  cursor is a position rather than an item, so a sandbox removed between two pages does not break
  the walk. Without `limit` the list is whole and `next` is `null`. With a limit, `next` is `null`
  only once nothing follows. A `limit` under 1, or a cursor that could never be a key of the list,
  is 400 `invalid_request`. The CLI never sets a limit, so `list` and the store lists print
  everything.
- `GET /v0/sandboxes` answers `{"sandboxes": [...], "next"}` the way `shard list` lists them, with
  stopped sandboxes hidden unless `all=true`. When some records are unreadable, it still answers 200
  with the readable sandboxes and a `warnings` array that holds one string per unreadable record.
  `list` prints the table, then the warnings on stderr, and exits non-zero. The route answers 500 only
  when the list itself failed.
- `GET /v0/sandboxes/{id}` takes an id or a name and answers the record. When the record names a
  policy, an `egress` object sits beside it with what the host enforces, as `shard inspect` prints
  it. With `?wait=true` the route holds until the record leaves `pending`. A caller then reads the
  `running` or `failed` state that a background create reached, rather than the `pending` it started
  with. A record that is not pending answers at once. The route answers 404 when nothing has the
  reference, which `inspect` prints as `no sandbox <ref>`, and 400 when the reference does not
  validate. It answers 500 for anything else: an unreadable record, a name link that points at
  something that is not a sandbox, or a policy that cannot be compiled.

- `POST /v0/sandboxes` takes `{"image", "snapshot", "name", "command", "env", "workdir", "user",
  "secrets", "policy", "resources": {"memory_mib", "vcpus", "disk_mib"}, "restart": {"policy",
  "retries", "backoff"}}` and answers 201 with the record, which keeps the digest of the image it
  runs over in `digest`. A `memory_mib` of 0 asks for no bound. A body names `image` or `snapshot`,
  never both, and the snapshot routes below say what a create from a snapshot does. `command` is the
  app, which `shard run` sends and `shard create` does not. The image's own ENTRYPOINT and CMD never
  run, so a body with no `command` starts only `shard-init`, and the sandbox stays up. A cached
  image needs no pull, so the create builds and starts the sandbox before it answers, and the record
  says `running`. A claim that fails at that point gives everything back, and the create answers
  500, or 422 `command_not_started` with `exit_code` when the app never started. An uncached image
  makes the record `pending`, and the create answers before the download. The daemon pulls, builds
  and starts behind it, and the record lands on `running` or `failed` with a one-line
  `failed_reason`. A background pull or start that fails is therefore read from the record, and does
  not come back as an error. With `?wait=true` the create holds until the record leaves `pending`,
  then answers the `running` or `failed` record it reached, so a caller reads the settled record
  without a poll. An app that never started is the exception: the wait answers 422
  `command_not_started` with `exit_code` and leaves no sandbox, or 500 when that sandbox could not
  be removed, while a create with no wait keeps the `failed` record. The plain create answers at
  once. A wait that sends `Accept: application/x-ndjson` streams the pull instead: one `{"event"}`
  line per step as it lands, then `{"sandbox"}` with the settled record, or a last `{"error"}` line
  for that refusal or that 500. The create is a public route, so an `{"event"}` line carries no
  `path`. The create answers 400 when the body does not decode, when a field does not validate, or
  when the body names a secret or a policy the host does not hold. It answers 400 `invalid_request`
  naming the user when `user` names a user or group the image does not list, on the plain create,
  the wait and the NDJSON wait, whose last line carries the error once an event is out. An uncached
  image is read only after the pull, so there the plain create answers the `pending` record and the
  user lands in the `failed_reason`, while both waits still answer the 400. It answers 409
  `name_taken` when another sandbox already holds the name.
- `POST /v0/sandboxes/{id}/start` takes no body and answers 200 with the record of the sandbox it
  started again. It answers 404 when nothing has the reference, and 409 when the sandbox is not
  stopped.
- `POST /v0/sandboxes/{id}/stop` takes no body. It sends SIGTERM to the entrypoint and answers 200
  with the stopped record as soon as the entrypoint exits. An entrypoint that is still running after
  30 s (`models.StopGrace`) is killed. Errors: 400 for a body with any field, `grace` included, 404,
  and 409 when the sandbox is not running.
- `DELETE /v0/sandboxes/{id}` answers 204 with no body. Errors: 404, and 409 when the sandbox is
  still up, unless the query has `?force=true`. Then the route stops the sandbox first, with the
  same 30 s grace.
- `POST /v0/sandboxes/{id}/pause` takes no body and answers 200 with the paused record. Errors: 404,
  and 409 when the sandbox is not running or when the provider does not claim the verb. A client
  that hangs up does not cut the pause. The daemon gives a pause at most 10 minutes
  (`DefaultPauseBudget`) before it cuts it. A pause that the substrate lost after its checkpoint
  began, including a cut one, answers 500 and leaves the record `failed` with the reason.
- `POST /v0/sandboxes/{id}/resume` takes no body and answers 200 with the running record. Errors:
  404, and 409 when the sandbox is not paused, when its record names no checkpoint, or for an
  unclaimed verb.
- `POST /v0/sandboxes/{id}/fork` takes `{"name"}` and answers 201 with the new record, which runs
  from a capture of the running source; the source runs on as it was (SHARD-457). Errors: 400 for a
  body that does not decode or a name that does not validate, 404, and 409 when the source is not
  running or for an unclaimed verb.

A snapshot is the files a stopped sandbox kept, with no memory image. It lives under
`<root>/snapshots/<id>`, has an id and an optional name, and outlives the sandbox it came from. A
snapshot name is unique among snapshots, and a sandbox may hold the same name. Every route that
takes `{ref}` takes an id or a name. A snapshot record reads like this, with the digest of the image
the source ran over in `digest` and the bytes the copy holds on the host in `size`:

```
{
  "id": "quiet-heron-4f2a",
  "name": "web-base",
  "source": "misty-otter-81c0",
  "source_name": "web",
  "image": "index.docker.io/library/alpine:3.20",
  "digest": "sha256:<digest>",
  "provider": "gvisor",
  "disk_mib": 10240,
  "memory_mib": 512,
  "size": 1048576,
  "created_at": "2026-10-04T09:00:00Z"
}
```

- `POST /v0/snapshots` takes `{"sandbox", "name"}`, where `sandbox` is an id or a name, and answers
  201 with the snapshot record. The provider copies the source's files: gVisor, runc and Sysbox copy
  the writable layer and `/tmp`, Firecracker reflinks the overlay disk, and `vz` makes an APFS clone
  of the disk image. Errors: 400 for a body that does not decode, a name that does not validate, a
  source whose image the host no longer holds, or a source whose tag the host now holds at another
  digest, 404, 409 `sandbox_not_stopped` when the source runs or is paused, and 409 `name_taken`
  when another snapshot holds the name.
- `GET /v0/snapshots` answers `{"snapshots": [...], "next"}`, ordered by id and paged like every
  other list.
- `GET /v0/snapshots/{ref}` answers the snapshot record. Errors: 404.
- `DELETE /v0/snapshots/{ref}` answers 204. A sandbox made from the snapshot holds its own copy, so
  nothing refuses the removal. Errors: 404.

`POST /v0/sandboxes` with `"snapshot"` in place of `"image"` starts a new sandbox from a copy of the
snapshot's files, under a new id and address. The new sandbox runs `shard-init` alone, so the body
takes no `command` and no `restart`. The snapshot holds no secrets and no policy, so the body names
its own. The create never pulls, so it builds and starts the sandbox before it answers, and the
record says `running` and names the snapshot's id in its `snapshot` key. It answers 400 when the
snapshot was made on another provider, or when the host no longer holds its image at the digest the
snapshot recorded. On Firecracker and `vz` a larger `disk_mib` grows the snapshot's disk, and a
smaller one answers 400, as a disk only grows. A body with no `disk_mib` or no `memory_mib` takes the
snapshot's, which is the bound the source ran under. A `memory_mib` of 0 is not an omitted one: it
asks for no bound. A snapshot reference that nothing has is 404.
An image that a snapshot names stays held: `DELETE /v0/images/{ref}` refuses it with 409 `in_use`,
and `image prune` leaves it.

- `POST /v0/sandboxes/{id}/exec` takes `{"command", "env", "workdir", "user", "stdin", "tty",
  "size": {"rows", "cols"}, "attach"}`, where `attach` holds the output for the first attach under the
  same 30 s bound. The route validates the body, starts the command at once, and answers 201 with
  the exec record once the command's `execve` took:
  `{"exec", "sandbox", "command", "state": "running"|"exited", "exit_status": {"code",
  "signal"} or null, "started_at", "exited_at", "truncated", "lost_bytes"}`. Errors: 400 for a body that does not decode or
  a request that names no command, 404, and 409 when no command can run in the sandbox. A command
  that is not there or cannot run answers 422 `command_not_started`, and the daemon keeps no record
  of it. A launch that 20 s (`DefaultExecStartBudget`) does not prove answers 504
  `timeout`, and the daemon ends the command.
- `GET /v0/sandboxes/{id}/exec` answers `{"execs": [...], "next"}` with every exec the sandbox holds.
- `GET /v0/sandboxes/{id}/exec/{exec-id}` answers the exec record. With `?wait=true` it holds the
  answer until the command ends, then answers the ended record. With the WebSocket handshake it
  answers 101 instead and attaches. It replays the buffered output, then streams live to the exit on
  stream 3. Errors: 404 when the exec ended with the sandbox, was evicted by the 32-exec cap, or
  belongs to another sandbox, and 409 `in_use` for a second attach. A drop leaves the command
  running, so a later attach replays the output again. A stalled client is closed with no exit, and
  the record says how much it lost.
- `POST /v0/sandboxes/{id}/exec/{exec-id}/kill` takes `{"signal": "TERM"|"KILL"}`, with TERM as the
  default. It signals the running command and answers 204. Errors: 404, and 409 `exec_exited` once
  the command ended. An exec that a signal ends reports code 128+n and signal 0 on every provider.
  Only the entrypoint's exit status carries the signal.
- `DELETE /v0/sandboxes/{id}/exec/{exec-id}` answers 204 and frees the record and its buffer.
  Errors: 404, and 409 `exec_running` while the command still runs.
- `POST /v0/sandboxes/{id}/exec/{exec-id}/resize` takes `{"rows", "cols"}` and answers 204. It
  answers 404 when the exec has no terminal, has ended or belongs to another sandbox. Only a `tty`
  exec has a terminal to resize.
- `GET /v0/sandboxes/{id}/attach` answers 200 with how the app ended, `{"code", "signal",
  "restarts"}`, once its restart policy ends. `code` is 128+n when a signal ended the app. With the
  WebSocket handshake it answers 101 instead. The app's output comes on stream 1 from the start of
  the log, stdout and stderr interleaved, then the exit on stream 3, and the daemon closes with 1000.
  A failure comes on stream 5. Errors, before anything is on the wire: 404, 409 `no_app` for a
  sandbox that `create` made, and 409 `sandbox_not_running`, also when the sandbox stops before the
  app ends. `shard run` uses the WebSocket.
- `POST /v0/sandboxes/{id}/app/stop` takes `{"force"}`, ends the app as the restart policy section
  says, and answers 204. Errors: 404, 409 `no_app`, 409 `app_ended` once the policy ended, and 409
  `sandbox_not_running`.
- `GET /v0/sandboxes/{id}/logs` answers 200 `text/plain; charset=utf-8` with everything the
  entrypoint wrote. A `pending` sandbox has written nothing yet, so it answers 200 with an empty
  body, as a `created` one does. Errors: 404, 409 `sandbox_failed`, and 400 for a `follow` that is
  not a boolean.
- `GET /v0/sandboxes/{id}/logs?follow=true` with the WebSocket handshake answers in binary messages.
  The log bytes come on stream 1. When the sandbox is gone, `{"reason": "stopped"|"removed"}` comes
  on stream 3 and the daemon closes with 1000. A failure comes on stream 5 instead. Without the
  handshake the route answers 200 with chunked `text/plain`, the bytes as they come. The body ends
  when the sandbox stops or is removed, so `curl -N` follows a log. A follow of a `pending` sandbox
  waits for the create: it follows once the sandbox is up, ends with `removed` when an rm takes it,
  and ends with a `sandbox_failed` failure on stream 5 when the create fails, or ends the body without
  the handshake. Either way, a 404 or a 409 `sandbox_failed` comes before anything is on the wire.
  `shard logs -f` uses the WebSocket.
- `GET /v0/sandboxes/{id}/egress-log` answers 200 with the newest 10000 egress decisions of the
  sandbox as a JSON array, oldest first. The array holds the proxy's own records and the host drops
  that the daemon wrote into the same file. The `Shard-Egress-Cut` header counts the older records
  the route left out, and is absent when it left out none. Errors: 404. `shard policy logs` prints
  one record per line.
- `GET /v0/sandboxes/{id}/egress-log?follow=true` with the handshake answers in text messages, one
  JSON record each, live. A stopped or removed sandbox ends the stream with close 1000 and the reason
  as the close text. A failure of the follow is close 1011 with the error. Without the handshake the
  route answers 200 with chunked `application/x-ndjson`, one record per line as it lands, and the
  body ends on the same stop or remove. Either way, a 404 comes before anything is on the wire.
- `PUT /v0/sandboxes/{id}/files?path=&mode=&user=&parents=` streams the body into the running guest,
  and answers 204 once the body sits at `path` as one file. The guest writes to a temp name beside
  the file, syncs it and renames it over the old one, so a put that dies midway leaves the old file
  whole. `mode` is the octal permission bits, 0644 by default and at most 0777. `user` resolves the
  same way as for an exec, with the entrypoint user as the default. The write runs as that user, who
  then owns the file. `parents=true` creates the missing directories. The body needs a
  `Content-Length`. The route answers 400 for a relative path, a chunked body, a bad mode or a write
  the guest refuses. It answers 404 for no sandbox or a missing directory, and 409 when the sandbox
  is not running.
- `GET /v0/sandboxes/{id}/files?path=` answers 200 `application/octet-stream` with the file and its
  `X-Shard-Stat`. The body is chunked to the end of the file and never cut at the stat's size,
  because a `/proc` file states its size as 0. A guest that fails after the 200 cuts the chunked body
  before its last chunk, so the client reads an unexpected EOF. Errors: 400 for a directory or
  anything else that is not a regular file, 404, and 409 as above.
- `HEAD /v0/sandboxes/{id}/files?path=` answers 200 with no body and `X-Shard-Stat:
  {"type", "size", "mode", "uid", "gid", "mtime"}`, `type` one of `file`, `dir`, `symlink` or
  `other`. It never follows a final symlink. A refusal carries only the status. `shard cp` uses all
  three of these file routes.
- `GET /v0/sandboxes/{id}/ls?path=` answers 200 `{"entries": [{"name", "type", "size", "mode", "uid",
  "gid", "mtime"}]}`, sorted by name, where each entry is its own lstat. It follows a final symlink
  to the directory. A guest that fails after the 200 leaves off the closing `]}`, so a cut listing
  never parses. Errors: 400 for a path that is not a directory, 404, and 409 as above.
- `POST /v0/sandboxes/{id}/mkdir` takes `{"path", "mode", "parents", "user"}` and answers 204. `mode`
  is an octal string, `"755"` by default and at most `"777"`, and it is set past the umask.
  `parents: true` works like `mkdir -p`. It makes the directories that lead to the path, and accepts
  a directory that is already there. `user` works as for a put. The route answers 400 for a bad
  mode, or for a path that is already there, unless `parents` is set and the path is a directory. It
  answers 404 for a missing parent, and 409 as above.
- `DELETE /v0/sandboxes/{id}/files?path=&recursive=` removes the path and answers 204. For a symlink,
  the route removes the link and never its target. A directory with anything in it needs
  `recursive=true`, and `/` is never deleted. The removal runs as the entrypoint user. Errors: 400
  for `/` or for a full directory without `recursive`, 404, and 409 as above.
- `PUT /v0/sandboxes/{id}/archive?path=&user=` unpacks the tar body, chunked or sized, under the
  guest directory `path`, and answers 204 once every entry is on disk. `user` works as for a put.
  The unpack runs as that user, who owns every entry. Each file lands through a temp name. The guest
  refuses an absolute name, a `..`, an entry under a symlink that leaves `path`, a hard link to a
  file the tar did not carry, a device node and a fifo. Directories take their modes once their
  entries are in, and a directory that is already there keeps its own mode. The body is held to the
  same idle bound as the body of a put. The route answers 400 for a refused entry, a body that is
  not a tar, or a `path` that is not a directory. It answers 404, and 409 as above.
- `GET /v0/sandboxes/{id}/archive?path=` answers 200 `application/x-tar` with the path and its
  `X-Shard-Stat`. The top entry is named after the path's base name, and a directory's entries come
  in lexical order. A guest that fails after the 200 aborts the chunked body, so a cut tar never
  reads as a whole one. Errors: 400 for `/`, 404, and 409 as above. `shard cp` copies a directory
  through both archive routes. A copy out unpacks what the sandbox sends, so it refuses an entry or a
  symlink that leaves the destination, a device node, and a hard link to a file the tar did not
  carry. It also drops setuid and setgid, and stops past 64 GiB or 1048576 entries.
- `POST /v0/sandboxes/{id}/secrets/{name}` grants a stored secret to a created or stopped sandbox and
  answers 200 with the record. The placeholder lands in the bundle environment, and the proxy CA
  lands in the writable layer. Errors: 404, 400 when the host holds no such secret or when the guest
  environment already holds that name, and 409 when the sandbox runs or is paused.
- `DELETE /v0/sandboxes/{id}/secrets/{name}` takes the grant and the placeholder back and answers 200
  with the record. The proxy CA stays. Errors: 404, and 409 when the sandbox runs or is paused.
- `PUT /v0/sandboxes/{id}/policy` takes `{"policy": "<name>"}`, gives a created or stopped sandbox
  that policy, and answers 200 with the record. The daemon writes the record and applies the host
  rules. If the host refuses the rules, the daemon puts the record back. A sandbox holds one policy,
  so this replaces the one it holds. Errors: 404, 400 when the host holds no such policy, and 409
  when the sandbox runs or is paused.
- `DELETE /v0/sandboxes/{id}/policy` leaves the sandbox with no policy and answers 200 with the
  record. The secrets it holds and the proxy CA stay. Errors: 404, and 409 when the sandbox runs or
  is paused.

- `GET /v0/policies` answers `{"policies": [...], "next"}`, and `GET /v0/policies/{name}` answers one
  policy with `holders`, the sandboxes whose record names it. The field is omitted when no sandbox
  names the policy. That is what `shard policy
  list` and `shard policy show` print. Errors: 404 when the host holds no such policy.
- `PUT /v0/policies/{name}` takes `{"rules": [{"action": "allow"|"deny", "rule": "<destination>"}]}`
  with the rules in the order they were given. It compiles them, stores the policy and re-applies it
  at once to every sandbox that names it. It answers 200 with the policy. The CLI never parses a
  rule, because the daemon owns the grammar. Errors: 400 for a name or a rule the host cannot
  enforce, and 500 when the store holds the new rules but the host still enforces the old ones. The
  error message says so.
- `DELETE /v0/policies/{name}` answers 204. Errors: 404, and 409 with the name of every sandbox that
  holds the policy. There is no force here, because a sandbox with no policy would have no egress
  rules at all.
- `GET /v0/secrets` answers `{"secrets": [...], "next"}` with the name, the destinations, the
  placeholder and the times of each secret, and never a value. Unreadable files come back in
  `warnings` beside the readable ones. `secret list` prints them on stderr before it exits non-zero.
- `PUT /v0/secrets/{name}` takes `{"value", "destinations", "mock"}` and answers 200 with the record.
  The record carries the placeholder and no value. Errors: 400 for a name, a destination or an empty
  value the host refuses, and 409 with the name of every sandbox that holds the placeholder a new
  `mock` would change.
- `DELETE /v0/secrets/{name}` answers 204. Errors: 404, and 409 with the name of every sandbox that
  was granted the secret, unless the query has `?force=true`.
- `GET /v0/images` answers `{"images": [...], "next"}`, with the images as `shard image list` prints
  them. An entry the daemon could not read carries its reason in `broken`.
- `POST /v0/images/pull` takes `{"ref"}`, pulls the image and answers 200 with it. Errors: 400 for
  a reference that does not parse, and 500 when the registry or the unpack failed. With `Accept:
  application/x-ndjson` it streams one `{"event"}` line per step, then `{"image"}`.

A streamed pull reports each step as it lands. `cached` means the image is already on disk.
`pulling` carries the reference, the digest, the layer count and their bytes. One `layer` per layer
carries its bytes and whether it was already on disk. `unpacking` carries the layer count, and one
`unpacked` per layer follows in manifest order with its position, when the tree is not on disk yet.
A `building` carries the path of each disk or EROFS image that a VM provider boots from. Then
`pulled` says where the image went. A refusal before the first line keeps its status and its JSON
body. After the first line the status is already sent, so a failure comes as a last `{"error"}`
line with the same `code` and `message`.
- `DELETE /v0/images/{ref}` takes the whole reference, including its slashes, and answers 200 with a
  `warnings` array that lists what the route could not delete under the store. Errors: 404, and 409
  `in_use` with every sandbox that references the image, or else every snapshot that does, unless
  the query has `?force=true`. For a snapshot the fix names `shard snapshot remove`.
- `POST /v0/images/prune` removes every image that no sandbox and no snapshot references, and answers
  `{"removed": [...], "warnings": [...]}`. When a record is unreadable, the route refuses with 500
  and does not guess, because an image a sandbox needs would be gone.

A stream is a WebSocket (RFC 6455) on the same route, opened with the standard handshake. Every
refusal comes before the 101, as a status and a JSON body. An exec carries binary messages. The
first byte of each message is the stream, and the rest is the payload. The client sends 0 (stdin)
and 4 (stdin closed, empty). The daemon sends 1 (stdout), 2 (stderr), 3 (exit, `{"code", "signal"}`,
plus `"lost_bytes"` when output was lost and `"error"` when the command never ran) and 5 (a failure
of the daemon's own, `{"error": {"code", "message"}}`, the same object every error body carries).
One payload is at most 1 MiB, and a longer write goes as several messages. Stream 3 or 5 ends the
session, and the daemon closes with 1000. A client that closes first leaves the command running,
so a client can re-attach to it by its exec id. A `tty` exec carries the guest's terminal on stream
1 alone, because a terminal has no second stream to keep apart. Ping and pong are the standard
ones. The two `?follow=true` routes also serve without the handshake, as a chunked body that ends
with the sandbox, so `curl -N` follows either of them. An exec attach does not serve that way.

A 409 body is the refusal as the CLI prints it: `sandbox <id> is <state>: <fix>`. A verb that the
provider does not claim also gets a 409: `provider <name> does not support <verb> on this host`.

Every error body is `{"error": {"code": "<code>", "message": "<message>"}}`. `message` is the line
the CLI prints, and `code` is what a program matches on. The table below lists every code. Anything
else that a refusal carries lives inside `error`, and the root never holds anything else.

| code | status | when |
|---|---|---|
| `invalid_request` | 400 | the body does not decode, a field does not validate, or a named secret, policy or image is unknown. Also the TCP front, when the request line does not parse as net/http parses it, and then the front dials nothing |
| `body_too_large` | 413 | a JSON body over 1 MiB. The daemon reads no further, and closes the connection after the answer |
| `not_found` | 404 | no sandbox, snapshot, policy, secret, image or exec has the reference, or no route has the path |
| `sandbox_not_running` | 409 | exec, pause, fork, attach or app stop on a sandbox that is not running, one the substrate no longer holds, or one whose substrate process does not answer |
| `sandbox_not_stopped` | 409 | start or remove without force on a sandbox that is up, remove without force on a paused one, whose checkpoint a resume needs, and snapshot create on any sandbox that is not stopped |
| `sandbox_not_paused` | 409 | resume on a sandbox that is not paused |
| `no_app` | 409 | attach or app stop on a sandbox that `create` made, which runs no app |
| `app_ended` | 409 | app stop once the restart policy of the app ended |
| `sandbox_failed` | 409 | any verb except a get or a `remove` on a create that ended `failed`. The message carries the public `failed_reason`, and `remove` frees the sandbox |
| `sandbox_live` | 409 | grant, ungrant, attach or detach while the sandbox runs or is paused |
| `no_checkpoint` | 409 | resume on a paused sandbox whose record names no checkpoint |
| `unsupported` | 409 | the provider does not claim the verb |
| `exec_exited` | 409 | a kill of an exec whose command already ended |
| `exec_running` | 409 | a delete of an exec whose command still runs |
| `in_use` | 409 | delete a policy, secret or image that sandboxes hold, delete an image that snapshots hold, or move the placeholder of a secret sandboxes hold. `error` then adds `"holders": [ids]`. Also a second attach of an exec, without holders |
| `command_not_started` | 422 | an exec, or a create's app, whose command never started: it is not there, it cannot run, or its interpreter is not there. The message names the command and the kernel's reason, never a host path. `error` then adds `"exit_code"`, 127 for a command that is not there and 126 for one that cannot run, as a shell answers |
| `name_taken` | 409 | a create whose `name` another sandbox already holds, or a snapshot create whose `name` another snapshot holds |
| `unauthorized` | 401 | the TCP front, when the request carries no valid bearer token, and then the front dials nothing |
| `forbidden` | 403 | the TCP front, when the token is valid but its scopes do not reach the route, and then the front dials nothing. Also the daemon, on a create that names a secret without `secret:*` or a policy without `policy:*` |
| `timeout` | 504 | a stop, remove or restart whose substrate status call did not answer within the budget, or an exec whose launch the substrate did not prove within 20 s. Retry it once the runtime frees. On gVisor, rm --force reclaims through the wedge instead. It SIGKILLs the sandbox's own runsc processes, which it finds by the sandbox's cgroup and by the sandbox id on their command line, then finishes the teardown. It answers this code only when that kill fails too |
| `internal` | 500 | anything else. A local route answers what the daemon got back. A public route answers only `the daemon could not complete the request; its log has the cause`, and the daemon log keeps the cause |

`services/client` decodes only that object into `*client.APIError`, with `Status`, `Code`,
`Message`, `Holders` and `ExitCode`. A caller therefore matches on the code with `errors.As`, never on the
text. A body of any other shape is quoted as it came, under `internal`.

The base path is `/v0`, and `/v0` may change until launch 1. SHARD-83 freezes the contract as `/v1`.

`docs/openapi.json` is the OpenAPI 3.1 spec of the public routes. The daemon builds it from the same
route table it serves, so every operation names its token scope in `x-shard-scope`, which is the
scope the TCP front checks. `make openapi` writes it, and `make test` fails while the committed file
differs. The daemon validates each request against the spec before a handler runs. A refusal names
the field, as `validation failed: expected number >= 1 (query.limit)`, and never echoes its value.
The local routes are not in the spec, as the TCP front never forwards them.

The typed side of these routes is `services/client`, hand-written over the socket. It has `Version`,
`ListSandboxes`, `GetSandbox`, `CreateSandbox`, `StartSandbox`, `StopSandbox`, `RemoveSandbox`,
`PauseSandbox`, `ResumeSandbox`, `ForkSandbox`, `CreateSnapshot`, `ListSnapshots`,
`InspectSnapshot`, `RemoveSnapshot`, `Exec`, `ResizeExec`, `AttachApp`, `StopApp`, `Logs`,
`ListPolicies`, `GetPolicy`, `SetPolicy`, `RemovePolicy`, `ListSecrets`, `SetSecret`,
`RemoveSecret`, `ListImages`, `PullImage`, `RemoveImage` and `PruneImages`. `Exec` creates the exec,
then opens the WebSocket over the same socket. `AttachApp`, `Logs` with follow and `FollowEgressLog`
hold their stream open for as long as the follow lasts. A stream takes no deadline, but the create
before it does. The CLI verbs call this client and nothing else. Each call that answers in full gets
30 s. The deadline is per request, not on the `http.Client`. A daemon that accepts and never answers
fails as `GET <route> on <socket>: no answer within 30s`. `CreateSandbox` sets no deadline, because
the pull inside it has none that the client could know. `PauseSandbox`, `ResumeSandbox` and
`ForkSandbox` set none either, because a checkpoint takes as long as the memory and the disk it
writes. `CreateSnapshot` and `RemoveSnapshot` set none, because they take as long as the files they
copy or delete. `StopSandbox` and `RemoveSandbox` add the 30 s grace to theirs.

## The TCP front

`shard serve` is how a client on another host reaches the daemon. It speaks plain HTTP behind a
proxy or a tunnel that terminates TLS, as "A proxy in front" below shows. It verifies the JWT in
`Authorization: Bearer <jwt>` against a signing key. It then dials `${root}/shard.sock` and copies
bytes both ways:

```
shard --root /var/lib/shard serve
```

The front listens on `127.0.0.1:2376` by default, so only a proxy on the same host reaches it.
`--listen` takes any other address. A token then crosses the network in clear text between the
proxy and the front, so the front logs a line at start when the address is not loopback.

It is a byte proxy and not an API. It reads the request line and the headers of a request only as
far as the auth header. It replays those bytes onto the socket, then splices the two connections.
So the WebSocket handshake of an exec, a `logs` follow or an `egress-log` follow, and every message
after it, pass through untouched, and every route above works unchanged. The front splits the
request line on the ASCII space alone, and checks the method and the version as net/http does, so
it reads the same route the daemon serves. A line that does not parse that way, such as one split by
a non-breaking space, gets a `400` with the code `invalid_request`. The front writes it before the
token check and before it dials anything. A bad or missing token gets a `401` with the code
`unauthorized`, written before anything is dialed, so an unauthenticated client never reaches the
daemon. A socket that does not answer gives a `502` with the code `internal`.

The front verifies HS256 alone. Each of these gets the same `401`: a token signed by another
algorithm, a token signed by another key, a token with no subject, a token with no id, an expired
token, a token whose id the ledger does not hold, and a revoked token. A token with no `exp` never
expires, because the front enforces `exp` only when the token carries one. Neither the front nor the
CLI ever logs a token or the signing key, and the front logs the subject of every request it lets
through.

The signing key is `<root>/auth/signing-key`, unless `--signing-key-file` names another file. The
first `shard serve` or `shard tokens mint` that finds no key at that default path creates it: 32
random bytes from the kernel, written as 64 hex characters, in a `0600` file inside an `auth`
directory of mode `0700`. Every later run reads that file, so `serve` and `mint` always use the
same key. Nothing rotates, overwrites or rewrites a key, and nothing changes the mode of an `auth`
directory that is already there. Two runs that start together on a root with no key end with one
key: each writes its own temporary file in `auth` and links it to the final name, a link never
replaces a file, and the run that loses reads the key of the run that won. Both commands find the
default key under `--root`, so a root other than `/var/lib/shard` needs the same `--root` on both.

A key that is there but unusable is refused, never replaced. The error names the path and the
fault: a file that everyone on the host can read, a file the run cannot read, something other than
a file, an empty file, or a key under 32 bytes, the width an HS256 key needs. No error, log line or
output holds the key. A file that `--signing-key-file` names must exist, and `serve` and every
`tokens` verb refuse a named file that is missing rather than create one. `openssl rand -hex 32`
prints a key that passes. `tokens list` and `tokens revoke` never create a key or the `auth`
directory.

The daemon never reads, creates or removes `<root>/auth`, so a daemon starts the same with or
without one, and a host that serves no TCP never has one. One exception comes from the data dir on
Firecracker: on a root that cannot clone a disk, the first daemon start mounts an xfs image over
the root, and it refuses a root that already holds entries, because the mount would hide them. On
such a host, start the daemon once before the first `tokens mint` or `serve`, or name a key outside
the root with `--signing-key-file`.

The front bounds a connection before it checks the token. From one source address, it holds at most
32 connections that have not shown a valid token yet. In total it holds at most
(soft `RLIMIT_NOFILE` - 64) / 2, read at start and never fewer than 32. A held connection costs two
file descriptors once it dials the daemon socket, and 64 stay for the listener, the logs and the
dials. When a bound is full, the front closes the next connection at once and logs the refusal, at
most a few lines a second per source. A connection leaves that count
once its token passes, so a client that holds many `logs` follows or exec sessions open is never
refused for them. A connection must send its whole request head within 10 s, or the front closes
it. An accept that runs out of file descriptors or memory waits from 5 ms up
to 1 s and tries again, so a flood of connections never ends the front.

The access control is TLS at the proxy, one signing key in a file, and a coarse scope on each
token. There is no user and no role yet.

A token carries a list of scopes. The front maps the route of each request to one capability, and
lets the request through only when a scope covers it. A token with no scopes, or one that carries
`*`, holds every verb. A token that names scopes reaches only the routes those scopes cover. Every
other route gets a `403` with the code `forbidden`, written before anything is dialed. The front
maps the request line to the capability over the daemon's own route patterns, so the front and the
daemon agree on what each request is. An unknown route gets a `403` too, and so does every local
route, with the same body, for every token. `GET /v0/version`, `GET /v0/capabilities` and
`GET /v0/scopes` answer any valid token. These are the six capabilities:

| capability | routes |
| --- | --- |
| `sandbox:read` | list, get, `logs`, `egress-log`, `attach`, `GET /v0/snapshots` and `GET /v0/snapshots/{ref}` |
| `sandbox:write` | create, start, stop, pause, resume, fork, `app/stop` and `POST /v0/snapshots` |
| `sandbox:delete` | `remove` and `DELETE /v0/snapshots/{ref}` |
| `exec` | every `exec` route, and every `files`, `ls`, `mkdir` and `archive` route |
| `secret:*` | every `secrets` route, and the grant and ungrant on a sandbox |
| `policy:*` | every `policies` route, and the policy of a sandbox |

The check is per request, not per connection. The front forces `Connection: close` on every
request it forwards, so a kept-alive connection cannot carry a second request past the one the
front verified. A WebSocket upgrade is the one exception, because its `Connection: Upgrade` is what
makes the handshake work. Once the front splices that connection, the daemon owns its lifetime. The
exception is narrow. The front skips the `Connection: close` rewrite only when all four handshake
headers are present, and it reads the daemon's status line first. A `101` gives the connection that
the WebSocket needs. A non-101 answer ends after that one response, so a request pipelined behind a
handshake that the daemon does not upgrade never reaches the daemon.

The route check is coarse. `create` maps to `sandbox:write` alone, but a create can name secrets
and a policy, which `secret:*` and `policy:*` otherwise guard. So the daemon checks the token's
scopes a second time. A create that names a secret needs `secret:*`, and a create that names a
policy needs `policy:*`. Without the scope it needs, the daemon answers `403` with the code
`forbidden` and names the missing scope before it builds anything. The front carries the token's
scopes to the daemon in an `X-Shard-Scopes` header. It stamps the header on every request it
forwards, and strips any copy the client sent, so a forged header can only remove a right. A request
with no such header reached the daemon socket directly. That socket is the operator's own channel,
and it keeps every right. A `fork` keeps the grants of the source sandbox by design, so it needs
only `sandbox:write`. A snapshot holds no grants, so a create from one names its own secrets and
policy, and the daemon checks the same two scopes as for any create.

A token is minted on the server, from the same signing key, and never over the API:

```
shard tokens mint --name build-agent --duration 24h
shard tokens mint --name reader --scopes sandbox:read,exec
```

`mint` prints one JSON object to stdout and exits. The object holds the token, its `expires_at`
(RFC 3339 in UTC, equal to the token's `exp`, and `null` when the token never expires), and the
`scopes` it carries.

```
{"token":"<jwt>","expires_at":"2026-09-18T15:40:00Z","scopes":["*"]}
```

It is a local verb, like `daemon` and `serve`. It never reaches the daemon, and the daemon never
sees the signing key. `--name` is the subject the front logs. `--duration` defaults to 0, which
mints a token with no `exp` that never expires. `--scopes` is a comma-separated list of the scopes
the token carries. An empty `--scopes` mints `["*"]`, which is every verb, so pass `--scopes` for
any token except an operator's. The verb refuses a scope that `shard tokens scopes` does not
list. The error lists the valid scopes, and nothing is recorded. A client takes the `token` field
in `SHARD_API_KEY`: `export SHARD_API_KEY=$(shard tokens mint --name ci | jq -r .token)`. When an
operator replaces the signing key, every token it signed stops verifying at once.

The front reads the signing key once, at start, so a new key needs a `shard serve` restart. That
restart ends its active proxy connections.

### Tokens

Every minted token carries a random 128-bit `jti`, and `mint` appends one record for it to a ledger.
The record holds the id, the subject, when the token was issued, when it expires, its scopes, and
whether it is revoked. The ledger sits beside the signing key file, at `serve.tokens` in the same
directory, so the ledger of the default key is `<root>/auth/serve.tokens`. `tokens mint`, `tokens
list`, `tokens revoke` and `serve` all use that path, and no flag moves it. `mint` creates the ledger
`0640` when it is absent, and refuses a ledger that everyone can read. It prints no token when it
cannot write the record.

```
shard tokens list
shard tokens revoke <id>
shard tokens revoke --name ci
```

`tokens list` lists every record with the status a request would see now: `active`, `revoked` or
`expired`. With no ledger yet, it prints the header alone, and `revoke` of an id reports that the
ledger holds no such token. `revoke` marks one token by its id, or every token of a subject with
`--name`. The next request that carries a revoked token gets a `401`. Both are local verbs, like
`mint`, and they never reach the daemon.

The front reloads the ledger when its size or its modification time changes, so a `revoke` takes
effect on the next request without a restart. The ledger must be a regular file.
The front also checks the ledger once per second for every active proxy connection, including
WebSocket streams and plain HTTP follows. A revoked token,
an absent token id, or a ledger read error closes both sides of that connection. A token with an
`exp` ends the connection at its expiry, independently of the ledger check. A token without an
`exp` still has the one-second ledger check. These closes detach the client; they do not stop an exec
or a sandbox. If the front cannot read the ledger at start, it does not start. If the ledger
vanishes while the front runs, every new request gets a `401`, and the next check ends active
connections.

`mint` and `revoke` take an advisory lock on the ledger, at `serve.tokens.lock` beside it, so
parallel revokes and a mint that races a revoke never lose a record. The front only reads, so it
takes no lock.

The split between the daemon and the front deliberately departs from dockerd and hypeman, which
bind TCP themselves. The shard daemon runs as root and owns the sandboxes, so the network-facing
process is a separate, unprivileged one. It runs from its own unit,
`packaging/systemd/shard-serve.service`, which is off unless it is installed on purpose:

```
groupadd --system shard
useradd --system --no-create-home --gid shard shard
systemctl restart shard
install -d -m2750 -o root -g shard /etc/shard
openssl rand -hex 32 > /etc/shard/serve.secret
chown root:shard /etc/shard/serve.secret && chmod 0640 /etc/shard/serve.secret
curl -fsSLO https://github.com/presmihaylov/shard/releases/latest/download/shard-serve.service
install -m0644 shard-serve.service /etc/systemd/system/shard-serve.service
systemctl daemon-reload
systemctl enable --now shard-serve
```

The daemon gives the socket and the root to the group only when it starts, so it restarts once the
group exists. The unit runs as `shard:shard`, which is the group the socket is given. It reads the
signing key from a root-owned `0640` file that the group can read, named with `--signing-key-file`.
Give every `tokens` verb for that front the same flag, so the token lands in the ledger the front
reads: `shard tokens mint --name ci --signing-key-file /etc/shard/serve.secret`. The setgid bit on
`/etc/shard` gives the ledger that a root `tokens mint` creates the group `shard`, so the front can
read it at `0640`. The account has no other privilege. It cannot read a state file, and the daemon
still applies every rule of every verb.

A script or a CI job reaches a front instead of the socket with two variables:

```
export SHARD_REMOTE=https://shard.example.com
export SHARD_API_KEY=<the token field of a shard tokens mint record>
shard list
```

`SHARD_API_KEY` is the one credential, the `token` field of the record that `shard tokens mint`
prints. The client sends it as the bearer token, so its scopes, its expiry and its revocation apply
unchanged. The client trims the whitespace around it, and an empty or blank value is unset. With a
remote and no key, the client refuses before it dials, and the error names `SHARD_API_KEY`. It
refuses a value with a control character inside, such as a newline, because no HTTP header carries
one. A wrong, revoked or expired key reaches the front, which answers `401` with "no valid bearer
token". No error or log line on either side holds a token.

`--remote`, or `SHARD_REMOTE`, is an `http` or an `https` url, and `--remote` wins when both are
set. An `https` url dials TLS, on port 443 when it names no port, and verifies the certificate. Use
it for anything that crosses a public network. Without `SHARD_CA_FILE` the host's own trust store
decides. A private CA or a self-signed certificate needs `SHARD_CA_FILE`, the certificate that
signed the proxy's own:

```
SHARD_CA_FILE=./company-ca.pem shard --remote https://shard.internal list
```

An `http` url dials plain TCP, on port 80 when it names no port, and nothing encrypts the bytes, the
bearer token among them. It suits `shard serve` on localhost, which speaks plain HTTP itself, or a
front reached through an encrypted VPN. Every command over it prints one warning to stderr, never
to stdout, so `--format json` stays clean. `SHARD_CA_FILE` with an `http` url is refused before the
client dials, and the error names both.

A Go program gets the same rules from `client.NewRemoteFromEnv` in `services/client`, which reads
`SHARD_REMOTE`, `SHARD_API_KEY` and `SHARD_CA_FILE`. `client.NewRemote` takes a host, a raw token
and the CA bytes. The warning is the CLI's own. The switch is one transport change inside
`services/client`, and nothing else changes. The typed calls, the messages and the errors stay the
same. It is also the one way a client off Linux drives sandboxes, because the daemon itself runs on
Linux alone.

### A proxy in front

The proxy must pass a WebSocket upgrade through and must not buffer a response, or an exec, a
`logs -f` and an `egress-log` follow stall. It carries the bearer token, so it must not log the
`Authorization` header. Each setup below gives the client an `https` url on port 443.

Caddy, with a certificate from a public CA for a public name:

```
shard.example.com {
	reverse_proxy 127.0.0.1:2376 {
		flush_interval -1
	}
}
```

Caddy passes a WebSocket upgrade through by itself, and `flush_interval -1` sends each chunk of a
`?follow=true` body on at once. On a host with no public name, `tls internal` inside the site block
gives Caddy a CA of its own. The client then names that CA's root in `SHARD_CA_FILE`. A Debian
package keeps it at `/var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt`.
`docs/https-devbox.md` runs this setup on a devbox, for the SDK tests.

Cloudflare Tunnel, with no inbound port open on the host:

```
tunnel: <tunnel id>
credentials-file: /etc/cloudflared/<tunnel id>.json
ingress:
  - hostname: shard.example.com
    service: http://127.0.0.1:2376
  - service: http_status:404
```

Tailscale Serve, for clients on the same tailnet, which use `https://<host>.<tailnet>.ts.net`:

```
tailscale serve --bg --https=443 http://127.0.0.1:2376
```

Behind any of these, the front sees each client as the proxy's own TCP connection, and it reads no
forwarded header. The bound of 32 connections per source therefore applies to all clients
together, so rate-limit a flood at the proxy.

## One daemon per root

The daemon takes an exclusive flock on `daemon.lock` under the root, and refuses to start while
another daemon holds it. The daemon is the single writer of the sandbox, image, secret and policy
stores, so none of them is contended between processes. The lock dies with the process. Beside the lock, the daemon writes `daemon.pid`
for newsyslog to signal on a Mac, and removes it on a clean exit. Nothing in shard reads that file.
Nothing probes either file to decide whether a daemon is up. A client that needs a daemon asks the
socket and reads the outcome.

## Supervision

Each piece of background work is a task. A task that returns an error is restarted with exponential
backoff and jitter, capped at a minute. A run that stays up long enough earns a fresh backoff, so a
crash loop backs off and a one-off crash restarts fast. A panic in a task is contained and counts as
a failure. A task that returns nil is done, and stays done until the daemon restarts. The `api` and
`proxy` tasks return an error when a listener dies, so they are restarted. They return nil only
when the daemon is ending.
