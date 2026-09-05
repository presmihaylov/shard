# The daemon

`shard daemon` is the resident process. It listens on `${root}/shard.sock`, a unix socket, and answers a
REST API there: `GET /v0/version`, `GET /v0/sandboxes` and `GET /v0/sandboxes/{id}` today, every verb in
time. The CLI moves onto that socket verb by verb (SHARD-125); until a verb has moved it works on the
same on-disk stores under the same lockfiles, daemon or no daemon, and no verb starts the daemon. The
daemon never binds TCP: a remote front that terminates TLS and checks a token is a separate,
unprivileged process (SHARD-129) that dials the socket. A resident root process is installed on
purpose, through the systemd unit in `packaging/systemd/shard.service`:

```
cp packaging/systemd/shard.service /etc/systemd/system/
systemctl enable --now shard
```

## The socket

The daemon removes a stale `shard.sock` on start and listens on a fresh one, mode `0660` and group
`shard` when that group exists, so a member reaches the API without root. Without the group the
socket is root-only, `0600`, and the daemon logs which it chose. The socket closes when the daemon
stops. The API is documented in `api/openapi.yaml`:

```
curl --unix-socket /var/lib/shard/shard.sock http://shard/v0/sandboxes
```

## What the daemon owns

Only the work that has to outlive a one-shot command:

- **Proxy supervision**: start the egress proxy, health-check it, restart it after a crash, and
  re-front every sandbox that depends on it.
- **Egress log rotation**: the per-sandbox `egress.jsonl` files grow without it.
- **The OOM watchdog**: a host OOM kill takes a sandbox's sentry, and only a resident process can
  bring it back.

It never owns the sandbox lifecycle: create, stop and every other verb act on the stores directly,
daemon or no daemon.

## The proxy under the daemon

The daemon serves the egress proxy in-process, as a supervised task, under the same `proxy/lock`
every proxy takes. While a one-shot proxy from a verb holds that lock the task fails on it, and the
backoff retry is the takeover: the run after that proxy dies wins the lock. A crash of the daemon's
own proxy ends the run and the restart is the recovery, on the same gateway and ports, so the host
rules that turn a fronted sandbox's 80 and 443 need no re-apply. The one-shot start stays, a verb
still refuses a fronted sandbox when no proxy comes up, and a dead proxy still fails closed.

## One daemon per root, and liveness

The daemon takes an exclusive flock on `daemon.lock` under the root and refuses to start while another
holds it. An `Alive` probe holds that lock for a moment itself, so a starting daemon outwaits a
contended lock briefly before it calls the holder a daemon. The same lock is the liveness signal: it dies with the process, so there is no stale pid file. That signal is advisory. `Alive` true means a daemon holds the lock right now,
and the caller must still tolerate it dying a moment later; anything that needs the daemon checks
outcomes, not liveness.

## Supervision

Each piece of background work is a task. A task that returns an error is restarted with exponential
backoff and jitter, capped at a minute; a run that stays up long enough earns a fresh backoff, so a
crash loop backs off and a one-off crash restarts fast. A panic in a task is contained and counts as
a failure. A task that returns nil is done and stays done until the daemon restarts.
