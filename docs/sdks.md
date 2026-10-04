# The SDKs

Two packages called `useshards` talk to `shard serve`: one on npm for TypeScript, in
`sdks/typescript`, and one on PyPI for Python, in `sdks/python`. The Python one has a blocking
`Shard` and an asyncio `AsyncShard` with the same verbs. Each package's README has the full guide.

Both read the same two settings. Mint a key on the host with `shard tokens mint`, then:

```
export SHARD_REMOTE=https://shard.example.com
export SHARD_API_KEY=<the token>
```

`SHARD_CA_FILE` trusts a private CA, for an https remote only.

## TypeScript

```ts
import { Shard } from "useshards";

const shard = new Shard();
const sandbox = await shard.create({ image: "docker.io/library/alpine:3" });
try {
  const result = await sandbox.exec(["uname", "-sr"]);
  console.log(result.exitCode, result.stdout);
} finally {
  await sandbox.remove({ force: true });
  shard.close();
}
```

## Python

```python
from useshards import Shard

with Shard() as shard:
    sandbox = shard.create("docker.io/library/alpine:3")
    try:
        result = sandbox.exec(["uname", "-sr"])
        print(result.exit_code, result.stdout)
    finally:
        sandbox.remove(force=True)
```

## Python on asyncio

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

## Build and install from this tree

```
cd sdks/typescript && npm ci && npm pack          # useshards-<version>.tgz
npm install <path to>/useshards-<version>.tgz     # in your project

cd sdks/python && uv build --wheel                # dist/useshards-<version>-py3-none-any.whl
pip install <path to>/useshards-<version>-py3-none-any.whl
```

## The release gate

`make sdk-gate` builds and installs both packages as a user would, then drives them against one
daemon through its public front. It needs Node 22 and uv, and three settings:

- `SHARD_REMOTE`, the front.
- `SHARD_API_KEY`, a token with every public scope: `sandbox:read`, `sandbox:write`,
  `sandbox:delete`, `exec`, `secret:*` and `policy:*`.
- `SHARD_SUITE_WILDCARD_KEY`, a `*` token, which must still reach no local route.

`SHARD_CA_FILE` trusts a private CA, `SHARD_SUITE_IMAGE` picks the image, and `SHARD_GATE_HOST_PATHS`
(colon separated) names more host paths no public answer may hold, such as the daemon's root.

It runs, in order:

- `install.*`: `npm pack` and `uv build`, each installed into a clean project.
- `examples.*`: each package's quickstart, against the daemon.
- `suite.*`: the shared suite of `sdks/suite/checks.txt`, once in TypeScript and once in each Python
  mode. Each run must report every check of that list, so the gate clears `SHARD_SUITE_ONLY` for it.
- `cross.*`: one SDK acts and the other observes. A nonzero exit answers a result in both. A cancel
  in one leaves the command running for the other to find. A command one starts, the other
  reconnects to. A file one writes, the other reads byte for byte.
- `capture.*`: a 20 MiB writer under an 8 MiB limit keeps exactly its last 8 MiB, in each mode.
- `leak.*`: no host path, no local-administration field and no field of `sdks/gate/host-fields.txt`
  in `docs/openapi.json` or in any public answer, where every public route of the spec must answer
  one probe with success. Every route of `sdks/gate/local-routes.txt` answers both tokens as an
  unknown route.

Each check prints `PASS` or `FAIL` with its name. Then each release claim prints `CLAIM <claim>:
PASS` only when every check that proves it passed in every run. The gate exits 1 on any `FAIL`.
`--only <check>` runs the installs and that one check. `--out <dir>` keeps the logs and every
recorded answer, with both tokens redacted.

CI runs it on every pull request, as the `sdk release gate` job: a daemon on the runc provider as
root, `shard serve`, and a TLS front on 127.0.0.1.

To run it from a Mac against devbox-shard2 (`docs/https-devbox.md`), mint the two tokens into their
own env files, copy them as in step 5 there, and never print them:

```
ssh devbox-shard2 sudo shard-https token gate 'sandbox:read,sandbox:write,sandbox:delete,exec,secret:*,policy:*'
ssh devbox-shard2 sudo shard-https token gate-wildcard '*'
set -a; . ~/.shard/devbox-shard2-gate.env; set +a
SHARD_SUITE_WILDCARD_KEY=$(sed -n 's/^SHARD_API_KEY=//p' ~/.shard/devbox-shard2-gate-wildcard.env) make sdk-gate
```
