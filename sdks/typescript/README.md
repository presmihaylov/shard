# useshards

The TypeScript SDK for [shard](https://github.com/presmihaylov/shard), the sandbox manager.

`useshards` is not on npm. Until its GitHub release is published, build it from a checkout of this
repository:

```sh
git clone https://github.com/presmihaylov/shard.git
cd shard/sdks/typescript
npm install && npm run build
```

To use that build in another project, run `npm install /path/to/shard/sdks/typescript` there.

Once the GitHub release is published, install its tarball:

```sh
npm install https://github.com/presmihaylov/shard/releases/download/sdk-typescript-v0.1.0/useshards-0.1.0.tgz
```

Node 20.3 or later. The one runtime dependency is `openapi-fetch`.

## Connect

The SDK talks to `shard serve` over `https`, or `http`, which sends the key unencrypted. Mint a key on the host with `shard tokens mint`, then:

```
export SHARD_REMOTE=https://shard.example.com
export SHARD_API_KEY=<the token>
```

An option beats the environment: `new Shard({ remote, apiKey })`. `caFile` (or `SHARD_CA_FILE`) trusts
a private CA, and an `http` remote refuses one. Each client on an `http` remote emits one Node warning,
in the CLI's words.

## Quickstart

```ts
import { Shard } from "useshards";

const shard = new Shard();
const sandbox = await shard.create({ image: "docker.io/library/alpine:3" });
try {
  const result = await sandbox.exec(["uname", "-sr"]);
  console.log(result.exitCode, result.stdout);

  await sandbox.files.write("/tmp/hello.txt", "hello\n");
  console.log((await sandbox.exec("cat /tmp/hello.txt")).stdout);
} finally {
  await sandbox.remove({ force: true });
  shard.close();
}
```

`examples/quickstart.ts` has it in full, with a background command and an app. Set
`SHARD_REMOTE` and `SHARD_API_KEY`, then run these commands from `sdks/typescript`:

```sh
npm install && npm run build
npx tsx examples/quickstart.ts
```

## What to know

- **`create()` runs no command.** It answers a running sandbox, or one in state `failed` with
  `info.failedReason`. Run commands in it with `exec()`.
- **A sandbox outlives its app.** `shard.run(image, command, { restart })` starts one app and answers
  an `App`. When the app exits, or after `app.stop()`, the sandbox stays running, so you can still exec
  into it. Only `stop()` or `remove()` ends it.
- **A string command runs under `/bin/sh -c`.** An array runs as it is.
- **`exec(command, { background: true })` answers a `Command`.** `wait()`, `kill()`, `writeStdin()`,
  `closeStdin()` and `resize()` act on it. `commands.get(id)` finds it again from another client.
- **An abort never kills the remote command.** An `AbortSignal` ends only this client's wait and
  lets go of the stream. The command runs on until it exits or you call `kill()`.
- **Output may hold only its end.** A result keeps the newest 8 MiB of stdout and stderr together
  (`outputLimitBytes`, 0 keeps none). `onStdout` and `onStderr` see every chunk as it arrives.
  `result.lostBytes` is different: it counts output the daemon dropped before any client read it.
  `logs()` is bounded on the daemon too, so a long app may hold only its end.
- **Files stream.** `files.download()` writes to a temporary file and renames it at the end, so a
  large file never sits in memory. `files.upload()` sends a local file with its length.
  `uploadDir()` and `downloadDir()` move a whole tree as a tar.
- **A verb the provider lacks throws `UnsupportedError`.** `shard.capabilities()` says which of the
  eight lifecycle verbs the server supports (create, start, stop, remove, pause, resume, fork and
  snapshot), before you call them.

## Errors

`ShardError` is the base of every error below, which covers the daemon, the transport and the settings. A bad
local argument throws the native error instead: an `upload` of a missing file throws `ENOENT`, and
`create()` with neither an image nor a snapshot throws `TypeError`.

| Error | When |
|---|---|
| `ConfigurationError` | A setting is missing or refused. The message never shows the API key, and names a CA file path or the remote when that is what to fix. |
| `ShardConnectionError` | The daemon is unreachable, or a stream ended before the command did. |
| `CommandNotStartedError` | The command never ran, as when its binary does not exist. |
| `APIError` | The daemon refused the request: `AuthenticationError` (401), `PermissionDeniedError` (403), `NotFoundError` (404), `InvalidRequestError` (400, 413), `ConflictError` (409), `UnsupportedError`, `ServerError` (5xx). |

`ProtocolError` is an answer the SDK cannot read, `UnknownLengthError` an upload of unknown size, and
`UnsafeArchiveError` a tar entry that would land outside its target. A non-zero exit code is not an
error. Read `result.exitCode`.

## License

Apache-2.0.

## Lists and errors

`shard.list()` returns `{ sandboxes, warnings }`. `shard.secrets.list()` returns
`{ secrets, warnings }`. The warnings name entries the daemon could not read. Both lists collect
warnings from every page, keeping each exact text once in first-seen order. Check them before you
treat the result as complete. Other lists return arrays.

An `APIError` can carry `holders`, the sandbox ids that prevent an operation such as secret removal.
The property is `undefined` when the error has no holders field.
