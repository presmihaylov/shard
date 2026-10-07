"""Run commands in a fresh sandbox, put a file in it, and wait on a supervised process."""

from useshards import Restart, Shard

IMAGE = "docker.io/library/alpine:3"


def main() -> None:
    with Shard() as shard:
        print(f"daemon {shard.version().version}, fork supported: {shard.capabilities().fork}")

        sandbox = shard.create(IMAGE)
        try:
            result = sandbox.exec(["uname", "-sr"])
            print(f"uname exited {result.exit_code}: {result.stdout.strip()}")

            sandbox.files.write("/tmp/hello.txt", "hello from the host\n")
            print(sandbox.exec("tr a-z A-Z < /tmp/hello.txt").stdout, end="")

            # A background command runs on while this client does other work.
            command = sandbox.exec("sleep 1; echo done", background=True)
            print(f"background command {command.id} said {command.wait().stdout.strip()}")

            # A process ends, but its sandbox runs on until it is stopped or removed.
            ticker = sandbox.run("for i in 1 2 3; do echo tick $i; done", name="ticker", restart=Restart(policy="no"))
            ended = ticker.wait(timeout=60)
            print(f"process {ticker.name} exited {ended.status.exit.code if ended.status.exit else None}")
            print(ticker.logs(), end="")
        finally:
            sandbox.remove(force=True)


if __name__ == "__main__":
    main()
