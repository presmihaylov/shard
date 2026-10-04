# useshards

The TypeScript SDK for [shard](https://github.com/presmihaylov/shard), the sandbox manager.

```
npm install useshards
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
`SHARD_REMOTE` and `SHARD_API_KEY`, then run it with `npx tsx examples/quickstart.ts`.

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

Every error derives from `ShardError`.

| Error | When |
|---|---|
| `ConfigurationError` | A setting is missing or refused. The message names the setting, never its value. |
| `ConnectionError` | The daemon is unreachable, or a stream ended before the command did. |
| `CommandNotStartedError` | The command never ran, as when its binary does not exist. |
| `APIError` | The daemon refused the request: `AuthenticationError` (401), `PermissionDeniedError` (403), `NotFoundError` (404), `InvalidRequestError` (400, 413), `ConflictError` (409), `UnsupportedError`, `ServerError` (5xx). |

`ProtocolError` is an answer the SDK cannot read, `UnknownLengthError` an upload of unknown size, and
`UnsafeArchiveError` a tar entry that would land outside its target. A non-zero exit code is not an
error. Read `result.exitCode`.

## License

Apache-2.0.
