"""The quickstart on asyncio: run commands in a fresh sandbox, put a file in it, and wait on an app.

Set SHARD_REMOTE and SHARD_API_KEY, then run: python examples/quickstart_async.py
"""

import asyncio

from useshards import AsyncShard

IMAGE = "docker.io/library/alpine:3"


async def main() -> None:
    async with AsyncShard() as shard:
        version, capabilities = await asyncio.gather(shard.version(), shard.capabilities())
        print(f"daemon {version.version} on {capabilities.provider}")

        sandbox = await shard.create(IMAGE)
        try:
            result = await sandbox.exec(["uname", "-sr"])
            print(f"uname exited {result.exit_code}: {result.stdout.strip()}")

            await sandbox.files.write("/tmp/hello.txt", "hello from the host\n")
            print((await sandbox.exec("tr a-z A-Z < /tmp/hello.txt")).stdout, end="")

            # Commands in one sandbox run side by side.
            results = await asyncio.gather(*(sandbox.exec(f"sleep 1; echo {n}") for n in range(3)))
            print("side by side:", " ".join(each.stdout.strip() for each in results))
        finally:
            await sandbox.remove(force=True)

        app = await shard.run(IMAGE, "for i in 1 2 3; do echo tick $i; done")
        try:
            # The app ends, but its sandbox stays until it is removed.
            print(f"app exited {(await app.wait(timeout=60)).exit_code}")
            print(await app.logs(), end="")
        finally:
            await app.sandbox.remove(force=True)


if __name__ == "__main__":
    asyncio.run(main())
