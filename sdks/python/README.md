# useshards

The Python SDK for [shard](https://github.com/presmihaylov/shard), the sandbox manager. `Shard` is for
blocking code and `AsyncShard` is for asyncio. Both have the same verbs.

`useshards` is not on PyPI yet, so install it from the repository:

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

- **A sandbox outlives its app.** `shard.run(image, command)` answers an `App`. When the app exits,
  its sandbox stays running, so you can still exec into it. Only `stop()` or `remove()` ends it.
- **A string command runs under `/bin/sh -c`.** A list runs as it is.
- **`exec(..., background=True)` answers a `Command`.** `wait()`, `kill()`, `write_stdin()` and
  `resize()` act on it. `commands.get(id)` finds it again from another client. In the foreground `stdin=` is
  the whole input; in the background `stdin=True` keeps the input open for `write_stdin()`.
- **A cancel never kills the remote command.** An asyncio cancel, a timeout or a dropped connection
  ends only the wait. The command runs on until it exits or you call `kill()`.
- **Output is capped.** A result keeps the newest 8 MiB across stdout and stderr together
  (`output_limit_bytes=`). `on_stdout=` and `on_stderr=` see every chunk as it arrives.
- **Files stream.** `files.download()` writes to a temporary file and renames it at the end, so a
  large file never sits in memory. An upload sends its length first: `files.upload()` takes a path,
  and `files.write()` takes bytes, a seekable file, or a stream with `size=`.

## Errors

Every exception derives from `ShardError`.

| Exception | When |
|---|---|
| `ConfigurationError` | A setting is missing or refused. The message names the setting, never its value. |
| `ShardConnectionError` | The daemon is unreachable, or a stream ended before the command did. |
| `CommandNotStartedError` | The command never ran, as when its binary does not exist. |
| `APIError` | The daemon refused the request: `AuthenticationError` (401), `PermissionDeniedError` (403), `NotFoundError` (404), `InvalidRequestError` (400, 413), `ConflictError` (409), `UnsupportedError`, `ServerError` (5xx). |

A non-zero exit code is not an exception. Read `result.exit_code`.

## The shared suite

`uv run python -m suite` runs the checks of `sdks/suite/checks.txt` against `SHARD_REMOTE`, with
`SHARD_API_KEY` and `SHARD_SUITE_WILDCARD_KEY` each a `"*"` token. `SHARD_SUITE_ONLY=name,name` runs a subset,
`SHARD_SUITE_MODE=sync|async` runs one mode, and `SHARD_SUITE_IMAGE` picks the image.

## License

Apache-2.0.
