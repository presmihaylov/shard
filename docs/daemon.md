# The daemon

`shard serve` is a resident peer of the CLI, not its server. Both work directly on the same on-disk
stores under the same lockfiles, so every CLI verb keeps working, unchanged, whether the daemon is up
or not. No verb requires it, and no verb starts it: a resident root process is installed on purpose,
through the systemd unit in `packaging/systemd/shard.service`:

```
cp packaging/systemd/shard.service /etc/systemd/system/
systemctl enable --now shard
```

## What serve owns

Only the work that has to outlive a one-shot command:

- **Proxy supervision**: start the egress proxy, health-check it, restart it after a crash, and
  re-front every sandbox that depends on it.
- **Egress log rotation**: the per-sandbox `egress.jsonl` files grow without it.
- **The OOM watchdog**: a host OOM kill takes a sandbox's sentry, and only a resident process can
  bring it back.

It never owns the sandbox lifecycle: create, stop and every other verb act on the stores directly,
daemon or no daemon.

## The proxy under serve

The daemon serves the egress proxy in-process, as a supervised task, under the same `proxy/lock`
every proxy takes. While a one-shot proxy from a verb holds that lock the task fails on it, and the
backoff retry is the takeover: the run after that proxy dies wins the lock. A crash of the daemon's
own proxy ends the run and the restart is the recovery, on the same gateway and ports, so the host
rules that turn a fronted sandbox's 80 and 443 need no re-apply. The one-shot start stays, a verb
still refuses a fronted sandbox when no proxy comes up, and a dead proxy still fails closed.

## One daemon per root, and liveness

`serve` takes an exclusive flock on `daemon.lock` under the root and refuses to start while another
holds it. An `Alive` probe holds that lock for a moment itself, so a starting serve outwaits a
contended lock briefly before it calls the holder a daemon. The same lock is the liveness signal: it dies with the process, so there is no socket and
no stale pid file. That signal is advisory. `Alive` true means a daemon holds the lock right now,
and the caller must still tolerate it dying a moment later; anything that needs the daemon checks
outcomes, not liveness.

## Supervision

Each piece of background work is a task. A task that returns an error is restarted with exponential
backoff and jitter, capped at a minute; a run that stays up long enough earns a fresh backoff, so a
crash loop backs off and a one-off crash restarts fast. A panic in a task is contained and counts as
a failure. A task that returns nil is done and stays done until the daemon restarts.
