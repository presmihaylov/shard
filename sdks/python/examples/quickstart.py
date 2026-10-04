"""Run commands in a fresh sandbox, put a file in it, and wait on an app."""

from useshards import Shard

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
        finally:
            sandbox.remove(force=True)

        app = shard.run(IMAGE, "for i in 1 2 3; do echo tick $i; done")
        try:
            # The app ends, but its sandbox stays until it is removed.
            print(f"app exited {app.wait(timeout=60).exit_code}")
            print(app.logs(), end="")
        finally:
            app.sandbox.remove(force=True)


if __name__ == "__main__":
    main()
