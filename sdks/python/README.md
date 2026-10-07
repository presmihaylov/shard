# useshards

The Python SDK for [shard](https://github.com/presmihaylov/shard), the sandbox manager. `Shard` is for
blocking code and `AsyncShard` is for asyncio. Both have the same verbs.

```
pip install useshards
```

To run the code on `main` instead, install it from the repository:

```
pip install "useshards @ git+https://github.com/presmihaylov/shard#subdirectory=sdks/python"
```

Python 3.11 or later. The runtime dependencies are HTTPX, attrs and typing-extensions.

## Connect

The SDK talks to `shard serve` over HTTPS, or over HTTP on localhost or a trusted encrypted network, where it
warns once per client as the CLI does. Mint a key on the host with `shard tokens mint`, then:

```
export SHARD_REMOTE=https://shard.example.com
export SHARD_API_KEY=<the token>
```

An argument beats the environment: `Shard(remote=..., api_key=...)`. `ca_file=` (or `SHARD_CA_FILE`) trusts a
private CA, for an https remote only. A remote with no port takes 443 for https and 80 for http.

## Quickstart

```python
from useshards import Shard

with Shard() as shard:
    sandbox = shard.create("docker.io/library/alpine:3")
    try:
        result = sandbox.exec(["uname", "-sr"])
        print(result.exit_code, result.stdout)

        sandbox.files.write("/tmp/hello.txt", "hello\n")
        print(sandbox.exec("cat /tmp/hello.txt").stdout)
    finally:
        sandbox.remove(force=True)
```

The same on asyncio:

```python
import asyncio
from useshards import AsyncShard

async def main() -> None:
    async with AsyncShard() as shard:
        sandbox = await shard.create("docker.io/library/alpine:3")
        try:
            results = await asyncio.gather(*(sandbox.exec(f"echo {n}") for n in range(3)))
            print([r.stdout for r in results])
        finally:
            await sandbox.remove(force=True)

asyncio.run(main())
```

`examples/` has both in full. Set `SHARD_REMOTE` and `SHARD_API_KEY`, then run `python examples/quickstart.py`.

## What to know

- **`create()` runs no command.** It returns a running sandbox, or one in state `failed` with
  `info.failed_reason`. Run commands in it with `exec()`.
- **A sandbox outlives its app.** `shard.run(image, command)` returns an `App`. When the app exits,
  its sandbox stays running, so you can still exec into it. Only `stop()` or `remove()` ends it.
- **A string command runs under `/bin/sh -c`.** A list runs as it is.
- **`exec(..., background=True)` returns a `Command`.** `wait()`, `kill()`, `write_stdin()`, `close_stdin()`
  and `resize()` act on it. `commands.get(id)` finds it again from another client. In the foreground `stdin=` is
  the whole input; in the background `stdin=True` keeps the input open for `write_stdin()`.
- **A cancel never kills the remote command.** An asyncio cancel, a timeout or a dropped connection
  ends only the wait. The command runs on until it exits or you call `kill()`.
- **Output may hold only its end.** A result keeps the newest 8 MiB of stdout and stderr together
  (`output_limit_bytes=`, 0 keeps none). `on_stdout=` and `on_stderr=` see every chunk as it arrives.
  `result.lost_bytes` is different: it counts output the daemon dropped before any client read it.
  `logs()` is bounded on the daemon too, so a long app may hold only its end.
- **Files stream.** `files.download()` writes to a temporary file and renames it at the end, so a
  large file never sits in memory. An upload sends its length first: `files.upload()` takes a path,
  and `files.write()` takes bytes, a seekable file, or a stream with `size=`.
- **A forward carries a host port to the sandbox's own 127.0.0.1.** `sandbox.ports.add(9000, 8000)`
  listens on the host's 127.0.0.1:9000, or on every interface with `public=True`. A running sandbox
  listens at once, any other from its next start. `ports.list()`, `ports.remove(9000)` and
  `shard.ports()` read and end them, and `create(..., ports=[PortForward(9000, 8000)])` forwards from
  the first start.
- **A verb the provider lacks raises `UnsupportedError`.** `shard.capabilities()` says which of the
  nine lifecycle verbs the daemon's provider supports (create, start, stop, remove, pause, resume,
  fork, snapshot and port) before you call them.

## Lists

`shard.list()` returns `SandboxList(sandboxes, warnings)`. `shard.secrets.list()` returns
`SecretList(secrets, warnings)`. The async client returns the same result types. The warnings name
entries the daemon could not read. Both lists collect warnings from every page, keeping each exact
text once in first-seen order. Check them before you treat the result as complete. Other lists return
plain lists.

## Errors

`ShardError` is the base of every exception below, which covers the daemon, the transport and the settings. A
bad local argument raises the native exception instead: an `upload` of a missing file raises
`FileNotFoundError`, and `create()` with neither an image nor a snapshot raises `ValueError`.

| Exception | When |
|---|---|
| `ConfigurationError` | A setting is missing or refused. The message never shows the API key, and names a CA file path or the remote when that is what to fix. |
| `ShardConnectionError` | The daemon is unreachable, or a stream ended before the command did. |
| `CommandNotStartedError` | The command never ran, as when its binary does not exist. |
| `APIError` | The daemon refused the request: `AuthenticationError` (401), `PermissionDeniedError` (403), `NotFoundError` (404), `InvalidRequestError` (400, 413), `ConflictError` (409), `UnsupportedError`, `ServerError` (5xx). An exec past the running-exec bound is a plain `APIError` with status 429 and code `exec_limit`. |

`ProtocolError` is an answer the SDK cannot read, `UnknownLengthError` an upload of unknown size or
one whose source changed size while it was sent, and `UnsafeArchiveError` a `download_dir` tar entry
the SDK refuses to land, as one outside the destination, named by `entry` and `reason`. A non-zero
exit code is not an exception. Read `result.exit_code`.

An `APIError` can carry `holders`, the sandbox ids that prevent an operation such as secret removal.
The attribute is `None` when the error has no holders field.

## License

Apache-2.0.
