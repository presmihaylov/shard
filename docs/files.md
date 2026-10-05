# Files

The file API puts a file into a running sandbox and reads one out, and it needs neither `tar` nor a
shell in the image. This page is the design of SHARD-285, as Pres ruled it. SHARD-286 to 288 build it.

## The serving path

`shard-init` serves every provider, through an exec. The daemon runs `[/.shard/init, files]`
through `Provider.Exec` and speaks one wire over the exec's stdin and stdout: a JSON header line,
then the bytes (`services/supervisor/filesexec.go`, `cmd/shard-init/files.go`). A container sandbox
already mounts the supervisor read-only at `/.shard/init`. A VM's `shard-init` is the initrd's
`/init`, so it answers that path with itself. The result is one wire, one guest implementation and
one set of tests, with no vsock port of its own, so port 5003 is retired.

The rejected alternative had the daemon read and write the sandbox's rootfs mount on the host
(`<root>/sandboxes/<id>/bundle/rootfs`, an overlay over the image). A VM would still need the guest
path, so that meant two code paths, and gVisor does not see writes made there.

### What gVisor does with a host write

Proved on kvmlabssh with `runsc release-20260928.0`, a shard binary built from main `d4e1b78`, and
one sandbox on `node:22`. Shard runs runsc with `--overlay2=none` and the default
`--file-access=exclusive`, which lets the sentry cache the file tree. In the table, "host" is a write
into the rootfs mount and "guest" is a `shard exec` read.

| Case | The guest sees |
| --- | --- |
| The guest writes a file, the host reads it | the new bytes |
| The host rewrites a file in place that the guest has read | the new bytes |
| The host writes a temp file and renames it over one the guest has read | **the old file**, while the host sees the new one |
| The host makes a file in a directory the guest has listed | **nothing**: `ls` omits it, `cat` says no such file |
| The host makes a file in a directory the guest never listed | the new file |
| The host appends to the file of the rename case | **the old file** with no append |

So host writes are not a coherent way to put a file on gVisor. The atomic put that SHARD-286 needs
(temp name, sync, rename) is exactly the case the guest never sees, and a file that the host makes
in a directory the guest has listed stays invisible. Making host writes coherent needs
`--file-access=shared`, which revalidates on every file op of every workload to serve a call that
is rare.

Sysbox and runc were not probed. They share the host kernel, so a host write would be coherent. But
Sysbox runs the sandbox in a user namespace, so a host write would have to pick the shifted uid
itself, which the guest path gets for free.

### The transport

`exec` stdio carries the bytes, so it was measured on the same sandbox:

| Path | 1 GiB in | 1 GiB out |
| --- | --- | --- |
| `runsc exec` stdio, straight | 0.51 s, sha matches | 2.40 s, sha matches |
| `shard exec` through the daemon | 2.16 s, sha matches | **sha differs** |

The daemon's exec record keeps the last 8 MiB of output and never blocks the guest, so a client that
lags past it loses the middle of the stream (SHARD-333). The file API therefore never goes through
that record. The daemon calls `Provider.Exec` itself and pipes the exec's stdio straight into the
HTTP body, so the HTTP connection supplies the backpressure.

### Why the guest serves it

The guest path gives every provider one wire and one guest implementation. It needs no `tar` in any
image and no change to the gVisor cache, and the uid of a written file comes from the guest the way
an exec's does. The cost is one exec per file op on a container substrate. A `runsc exec cat` of a
64 MiB file took 43 ms end to end, start included.

## Stopped and paused sandboxes

A stopped and a paused sandbox both get 409 `sandbox_not_running`. A stopped one says
`sandbox <id> is stopped: start it again with shard start <name>`, and a paused one says
`sandbox <id> is paused: resume it with shard resume <name>`, where `<name>` is the id of a sandbox
created without one. Every file op needs a running guest.
Host access to a stopped sandbox's writable layer, the way `snapshot create` reads it, means a
different layout on each provider: an overlay upper on gVisor, Sysbox and runc, and a disk image on
Firecracker and vz.
On a paused gVisor sandbox, a host write would meet the cached tree of the restored sentry, which is
the stale case above. A later cut can add host access per provider.

## Who owns a written file

A put, a mkdir and an archive put take `user`, an image-style name or id. It is resolved against
the sandbox's own rootfs exactly as `ExecSpec.User` is. Empty means the user the
entrypoint runs as. The op runs as that user, so the guest kernel checks the permission and sets the
owner of the file. A put into `/etc` as a non-root user fails the way `exec` would, and `user=root`
writes anywhere. A stat reports the uid and gid it finds.

## Routes

Every route sits under `/v0/sandboxes/{id}`, where `{id}` is an id or a name. An error uses the
daemon's one envelope, `{"error":{"code":"<code>","message":"<message>"}}`, as `docs/daemon.md`
defines it. The path must be absolute, because shard has no default directory.

| Route | Body | Answer | Ticket |
| --- | --- | --- | --- |
| `PUT /files?path=&mode=&user=&parents=` | the file, streamed | 204 | 286 |
| `GET /files?path=` | | 200, the file, streamed | 286 |
| `HEAD /files?path=` | | 200, `X-Shard-Stat` `{type,size,mode,uid,gid,mtime}` | 286 |
| `GET /ls?path=` | | 200 `{"entries":[{name,type,size,mode,uid,gid,mtime}]}` | 287 |
| `POST /mkdir` | `{path,mode?,parents?,user?}` | 204 | 287 |
| `DELETE /files?path=&recursive=` | | 204; a non-empty dir without `recursive` refuses | 287 |
| `PUT /archive?path=&user=` | a tar, streamed | 204 | 288 |
| `GET /archive?path=` | | 200, a tar, streamed; the client unpacks it under the rule below | 288 |

The errors are `not_found`, `invalid_request` (a relative path, a bad tar, a tar entry that escapes
`path`), `sandbox_not_running`, `sandbox_failed` for a create that ended `failed`, and `internal`.
The file API adds no new error code. A client that refuses an entry of a `GET /archive` tar fails
with an error that names the entry, and writes nothing more.

The client unpacks a `GET /archive` tar as hostile. The guest packs it, so `shard cp
<id>:<dir> <dst>` guards the operator's machine the way the guest guards its own unpack. It unpacks
under `os.OpenRoot(dst)`, so no symlink reaches out of `dst`, and on a Mac neither does a name that
differs only in case. It refuses an absolute name, a `..`, a hardlink to a name outside the archive,
and a device node. It drops setuid and setgid, and it never chowns. The same rule binds the Python
and TypeScript clients.

Every route needs the `exec` scope. A write can replace what the entrypoint runs, and a read sees
what an exec `cat` sees, so a file route is as strong as exec (`services/serve/scopes.go`). A
separate `files` capability is worth adding only for a token that copies files and may not exec.

The CLI verb is `shard cp <src> <id>:<path>` and `shard cp <id>:<path> <dst>`, the same shape as
Docker's.

## What the file API never does

- It never buffers a file in the daemon, and never caps its size, because both directions stream.
- It never substitutes a secret. A file that holds a placeholder is bytes like any other.
- It never leaves a half-written file. A put writes a temp name in the target directory, syncs it
  and renames it over the old one, so a put that dies midway leaves the old file whole.
