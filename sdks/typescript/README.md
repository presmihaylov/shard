# useshards

The TypeScript SDK for [shard](https://github.com/presmihaylov/shard), the sandbox manager. Create
sandboxes on a shard host, run commands and processes in them, move files, forward ports, and
snapshot, fork or pause them.

[Docs](https://useshards.com/docs/sdks/typescript/) ·
[Python SDK](https://github.com/presmihaylov/shard/tree/main/sdks/python) ·
[Changelog](https://github.com/presmihaylov/shard/blob/main/sdks/typescript/CHANGELOG.md)

## Install

```sh
npm install useshards
```

## Connect

Point the SDK at your shard server and its API key:

```sh
export SHARD_REMOTE=https://shard.example.com
export SHARD_API_KEY=<the API key>
```

Or pass them in code: `new Shard({ remote, apiKey })`. No server yet? [Connect to a remote host](https://useshards.com/docs/guides/remote/) sets one up and
gives you the key.

## Quickstart

```ts
import { Shard } from "useshards";

const shard = new Shard();
const sandbox = await shard.create({ image: "docker.io/library/alpine:3" });

const result = await sandbox.exec(["uname", "-sr"]);
console.log(result.exitCode, result.stdout);

await sandbox.files.write("/tmp/hello.txt", "hello\n");
console.log((await sandbox.exec("cat /tmp/hello.txt")).stdout);

await sandbox.remove({ force: true });
shard.close();
```

## Docs

[The TypeScript SDK docs](https://useshards.com/docs/sdks/typescript/) cover the rest: sandboxes,
commands, processes, files, ports, secrets, policies, snapshots and
[errors](https://useshards.com/docs/sdks/errors/).

## License

Apache-2.0.
