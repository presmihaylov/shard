# Files

The file API puts a file into a running sandbox and reads one out, with no `tar` and no shell in the
image. This page is the design ruling of SHARD-285. Nothing on it is built yet: SHARD-286 to 288 build
it after Pres rules on the serving path.

## The serving path

A VM already has a channel: `shard-init` serves `stat`, `put` and `get` on vsock port 5003, one
connection per op, a JSON header line and then the bytes (`services/supervisor/files.go`,
`cmd/shard-init/files.go`). gVisor, Sysbox and runc have none. Two ways to give them one:

**Option A: `shard-init` serves every provider.** On a container substrate the daemon runs the
supervisor that every sandbox already mounts read-only at `/.shard/init`, through `Provider.Exec`
with `[/.shard/init, files]`, and speaks the same wire over the exec's stdin and stdout. The host
side in `services/supervisor` takes a `Dialer`, so a VM dials the port and a container's dial starts
that exec: one wire, one guest implementation, one set of tests.

**Option B: the host reads and writes the rootfs.** The daemon opens the sandbox's rootfs mount on the
host (`<root>/sandboxes/<id>/bundle/rootfs`, an overlay over the image) and writes there directly.
A VM still needs option A, so B means two code paths.

### What gVisor does with a host write

Proved on kvmlabssh, `runsc release-20260928.0`, a binary built from main `d4e1b78`, one sandbox on
`node:22`. Shard runs runsc with `--overlay2=none` and the default `--file-access=exclusive`, which
lets the sentry cache the file tree. "Host" is a write into the rootfs mount, "guest" is a
`shard exec` read.

| Case | The guest sees |
| --- | --- |
| The guest writes a file, the host reads it | the new bytes |
| The host rewrites a file in place that the guest has read | the new bytes |
| The host writes a temp file and renames it over one the guest has read | **the old file**, while the host sees the new one |
| The host makes a file in a directory the guest has listed | **nothing**: `ls` omits it, `cat` says no such file |
| The host makes a file in a directory the guest never listed | the new file |
| The host appends to the file of the rename case | **the old file** with no append |

So option B on gVisor is refuted. The atomic put that SHARD-286 needs (temp name, sync, rename) is
exactly the case the guest never sees, and a host-made file in a directory the guest has listed stays
invisible. Making B work needs `--file-access=shared`, which revalidates on every file op of every
workload to serve a call that is rare.

Sysbox and runc were not probed. They share the host kernel, so a host write would be coherent, but
Sysbox runs the sandbox in a user namespace, so a host write must pick the shifted uid itself, which
A gets for free.

### The transport A rides

`exec` stdio carries the bytes, so it was measured on the same sandbox:

| Path | 1 GiB in | 1 GiB out |
| --- | --- | --- |
| `runsc exec` stdio, straight | 0.51 s, sha matches | 2.40 s, sha matches |
| `shard exec` through the daemon | 2.16 s, sha matches | **sha differs** |

The daemon's exec record keeps the last 8 MiB of output and never blocks the guest, so a client that
lags past it loses the middle of the stream (SHARD-333). Option A must therefore not ride that
record: the daemon calls `Provider.Exec` itself and pipes the exec's stdio straight into the HTTP
body, with the backpressure of the HTTP connection.

### Ruling asked for: option A

One wire and one guest implementation on every provider, no `tar` in any image, no gVisor cache
change, and the uid of a written file comes from the guest the way an exec's does. The cost is one
exec per file op on a container substrate: a `runsc exec cat` of a 64 MiB file took 43 ms end to
end, start included.

## Stopped and paused sandboxes

**First cut: 409 `sandbox_not_running`, with `start it first`, for both.** Every file op needs a
running guest under option A. Host access to a stopped sandbox's writable layer, the way `clone`
reads it, is a different layout on each provider (an overlay upper on gVisor, Sysbox and runc, a disk
image on Firecracker and vz), and on a paused gVisor sandbox a host write would meet the cached tree
of the restored sentry, the stale case above. A later cut can add it per provider.

## Who owns a written file

`user` on a put, a mkdir and an archive put, an image-style name or id, resolved against the
sandbox's own rootfs exactly as `ExecSpec.User` is. Empty means the user the entrypoint runs as. The
op runs as that user, so the guest kernel checks the permission and owns the file: a put into `/etc`
as a non-root user fails the way `exec` would, and `user=root` writes anywhere. A stat reports the
uid and gid it finds.

## Routes

Under `/v0/sandboxes/{id}`, JSON errors `{"error", "code"}`, `{id}` an id or a name. The path is
absolute; shard has no default directory.

| Route | Body | Answer | Ticket |
| --- | --- | --- | --- |
| `PUT /files?path=&mode=&user=&parents=` | the file, streamed | 204 | 286 |
| `GET /files?path=` | | 200, the file, streamed | 286 |
| `HEAD /files?path=` | | 200, `X-Shard-Stat` `{type,size,mode,uid,gid,mtime}` | 286 |
| `GET /ls?path=` | | 200 `{"entries":[{name,type,size,mode,mtime}]}` | 287 |
| `POST /mkdir` | `{path,mode?,parents?,user?}` | 204 | 287 |
| `DELETE /files?path=&recursive=` | | 204; a non-empty dir without `recursive` refuses | 287 |
| `PUT /archive?path=&user=` | a tar, streamed | 204 | 288 |
| `GET /archive?path=` | | 200, a tar, streamed | 288 |

Errors: `not_found`, `invalid_request` (a relative path, a bad tar, a tar entry that escapes `path`),
`sandbox_not_running`, `internal`. No new code.

**Scope: `exec` for every route.** A write can replace what the entrypoint runs and a read sees what
an exec `cat` sees, so a file route is as strong as exec (`services/serve/scopes.go`). A separate
`files` capability is worth adding only for a token that copies files and may not exec.

The CLI verb is `shard cp <src> <id>:<path>` and `shard cp <id>:<path> <dst>`, the Docker shape.

## What the file API never does

- **Never buffers a file in the daemon**, and never caps its size: both directions stream.
- **Never substitutes a secret.** A file that holds a placeholder is bytes like any other.
- **Never leaves a half-written file.** A put writes a temp name in the target directory, syncs it
  and renames it over the old one, so a put that dies midway leaves the old file whole.
