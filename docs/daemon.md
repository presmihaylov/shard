# The daemon

`shard daemon` is the resident process beside the CLI. Both work directly on the same on-disk
stores under the same lockfiles, so every CLI verb keeps working, unchanged, whether the daemon is up
or not. No verb requires it, and no verb starts it: a resident root process is installed on purpose,
through the systemd unit in `packaging/systemd/shard.service`:

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

It never owns the sandbox lifecycle: create, stop and every other verb act on the stores directly,
daemon or no daemon.

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

The routes, this slice read only, with `api/openapi.yaml` as the spec of record:

```
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/version
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/sandboxes
curl --unix-socket /var/lib/shard/shard.sock 'http://localhost/v0/sandboxes?all=true'
curl --unix-socket /var/lib/shard/shard.sock http://localhost/v0/sandboxes/<id or name>
```

- `GET /v0/version` answers `{"version": "..."}`, the string `shard version` prints.
- `GET /v0/sandboxes` answers `{"sandboxes": [...]}` as `shard ls` lists them: stopped sandboxes
  hidden unless `all=true`. When some records are unreadable it still answers 200 with the readable
  sandboxes and a `warnings` array, one string per unreadable record. It answers 500 only when the
  list itself failed.
- `GET /v0/sandboxes/{id}` takes an id or a name. 404 when nothing has it, 400 when the reference
  does not validate, 500 for anything else: an unreadable record, or a name link at something that
  is not a sandbox.

Every error body is `{"error": "<message>"}`.

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
