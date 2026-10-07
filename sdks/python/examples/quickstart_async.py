"""The quickstart on asyncio."""

import asyncio

from useshards import AsyncShard, Restart

IMAGE = "docker.io/library/alpine:3"


async def main() -> None:
    async with AsyncShard() as shard:
        version, capabilities = await asyncio.gather(shard.version(), shard.capabilities())
        print(f"daemon {version.version}, fork supported: {capabilities.fork}")

        sandbox = await shard.create(IMAGE)
        try:
            result = await sandbox.exec(["uname", "-sr"])
            print(f"uname exited {result.exit_code}: {result.stdout.strip()}")

            await sandbox.files.write("/tmp/hello.txt", "hello from the host\n")
            print((await sandbox.exec("tr a-z A-Z < /tmp/hello.txt")).stdout, end="")

            # Commands in one sandbox run side by side.
            results = await asyncio.gather(*(sandbox.exec(f"sleep 1; echo {n}") for n in range(3)))
            print("side by side:", " ".join(each.stdout.strip() for each in results))

            # A process ends, but its sandbox runs on until it is stopped or removed.
            ticker = await sandbox.run(
                "for i in 1 2 3; do echo tick $i; done", name="ticker", restart=Restart(policy="no")
            )
            ended = await ticker.wait(timeout=60)
            print(f"process {ticker.name} exited {ended.status.exit.code if ended.status.exit else None}")
            print(await ticker.logs(), end="")
        finally:
            await sandbox.remove(force=True)


if __name__ == "__main__":
    asyncio.run(main())
