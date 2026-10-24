# useshards

The Python SDK for [shard](https://github.com/presmihaylov/shard), the sandbox manager. Create
sandboxes on a shard host, run commands and processes in them, move files, forward ports, and
snapshot, fork or pause them. `Shard` is for blocking code and `AsyncShard` is for asyncio. Both
have the same verbs.

[Docs](https://useshards.com/docs/sdks/python/) ·
[TypeScript SDK](https://github.com/presmihaylov/shard/tree/main/sdks/typescript) ·
[Changelog](https://github.com/presmihaylov/shard/blob/main/sdks/python/CHANGELOG.md)

## Install

```sh
pip install useshards
```

## Connect

Point the SDK at your shard server and its API key:

```sh
export SHARD_REMOTE=https://shard.example.com
export SHARD_API_KEY=<the API key>
```

Or pass them in code: `Shard(remote=..., api_key=...)`. No server yet? [Connect to a remote host](https://useshards.com/docs/guides/remote/) sets one up and
gives you the key.

## Quickstart

```python
from useshards import Shard

with Shard() as shard:
    sandbox = shard.create("docker.io/library/alpine:3")

    result = sandbox.exec(["uname", "-sr"])
    print(result.exit_code, result.stdout)

    sandbox.files.write("/tmp/hello.txt", "hello\n")
    print(sandbox.exec("cat /tmp/hello.txt").stdout)

    sandbox.remove(force=True)
```

## Docs

[The Python SDK docs](https://useshards.com/docs/sdks/python/) cover the rest: sandboxes, commands,
processes, files, ports, secrets, policies, snapshots, asyncio and
[errors](https://useshards.com/docs/sdks/errors/).

## License

Apache-2.0.
