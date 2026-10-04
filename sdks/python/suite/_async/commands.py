from __future__ import annotations

from useshards import AsyncCommand, CommandNotStartedError, TerminalSize

from .._shared import equal, matches, ok
from .harness import AsyncContext, Check, rejects, wait_for
from .mode import Printed, cancel_after


async def state(command: AsyncCommand) -> str:
    return (await command.inspect()).state


async def argv(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    result = await sandbox.exec(["printf", "%s", "hello world"])
    equal(result.exit_code, 0)
    equal(result.stdout, "hello world", "a list runs as argv, with no shell to split it")
    equal(result.stderr, "")


async def shell(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    result = await sandbox.exec("printf '%s' hello; echo oops >&2; echo $((1 + 2))")
    equal(result.exit_code, 0)
    equal(result.stdout, "hello3\n", "a string runs under /bin/sh -c")
    equal(result.stderr, "oops\n")


async def env_workdir_user(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    result = await sandbox.exec(
        'echo "$GREETING"; pwd; id -u', env={"GREETING": "hi there"}, workdir="/tmp", user="nobody"
    )
    equal(result.stdout, "hi there\n/tmp\n65534\n")


async def stdin(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    equal((await sandbox.exec(["cat"], stdin="from a string\n")).stdout, "from a string\n")
    result = await sandbox.exec("od -An -tu1", stdin=bytes([0, 1, 2, 250, 255]))
    equal(",".join(result.stdout.split()), "0,1,2,250,255", "stdin carries bytes as they are")
    open_ = await sandbox.exec(["cat"], background=True, stdin=True)
    await open_.write_stdin("first ")
    await open_.write_stdin(b"second")
    await open_.close_stdin()
    equal((await open_.wait()).stdout, "first second", "close ends the input, so cat ends")


async def tty_resize(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    first = Printed("24 80")
    command = await sandbox.exec(
        "stty size; read line; stty size",
        background=True,
        stdin=True,
        tty=TerminalSize(rows=24, cols=80),
        on_stdout=first.on_stdout,
    )
    await first.seen()
    await command.resize(40, 120)
    await command.write_stdin("\n")
    result = await command.wait()
    equal(result.exit_code, 0)
    matches(result.stdout, r"24 80\r?\n[\s\S]*40 120", "the second stty sees the new size")
    equal(result.stderr, "", "a terminal sends everything as stdout")


async def nonzero_result(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    result = await sandbox.exec("echo partial; exit 3")
    equal(result.exit_code, 3, "a nonzero exit is a result, not an error")
    equal(result.stdout, "partial\n")
    equal((await sandbox.exec(["false"])).exit_code, 1)


async def not_started_named(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    err = await rejects(CommandNotStartedError, lambda: sandbox.exec(["/no/such/binary"]))
    equal(err.exit_code, 127)
    matches(err.reason, r"no/such/binary")
    result = await sandbox.exec("/no/such/binary")
    equal(result.exit_code, 127, "the shell started, so a missing binary under it is a result")


async def background_wait_kill(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    quick = await sandbox.exec("sleep 1; echo done", background=True)
    ok(len(quick.id) > 0, "a background command has an id once it started")
    result = await quick.wait()
    equal(result.exit_code, 0)
    equal(result.stdout, "done\n")
    long = await sandbox.exec(["sleep", "300"], background=True)
    equal(await state(long), "running")
    await long.kill()
    # The exit is the code alone, 128 plus the signal, on every provider (SHARD-432).
    termed = await long.wait()
    equal((termed.exit_code, termed.signal), (143, None), "kill sends TERM by default")
    hard = await sandbox.exec(["sleep", "300"], background=True)
    await hard.kill("KILL")
    killed = await hard.wait()
    equal((killed.exit_code, killed.signal), (137, None))


async def list_get(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    command = await sandbox.exec(["sleep", "300"], background=True)
    entry = next((each for each in await sandbox.commands.list() if each.id == command.id), None)
    if entry is None:
        raise AssertionError("list holds the running command")
    equal(entry.command, ("sleep", "300"))
    equal(entry.state, "running")
    got = await sandbox.commands.get(command.id)
    equal(got.id, command.id)
    equal(await state(got), "running")
    await got.kill()

    async def exited() -> bool:
        return await state(command) == "exited"

    await wait_for("the killed command to end", 10, exited)
    ended = next((each for each in await sandbox.commands.list() if each.id == command.id), None)
    if ended is None:
        raise AssertionError("list still holds the killed command")
    equal((ended.exit_code, ended.signal), (143, None))


async def reconnect(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    one = Printed("one")
    command = await sandbox.exec("echo one; sleep 2; echo two", background=True, on_stdout=one.on_stdout)
    await one.seen()
    await command.disconnect()
    await command.reconnect()
    result = await command.wait()
    equal(result.exit_code, 0)
    equal(result.stdout, "one\ntwo\n", "a reconnect replays what the daemon holds, then streams the rest")


async def disconnect(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    marker = ctx.name("disconnect")
    command = await sandbox.exec(f"sleep 2; echo {marker} > /tmp/{marker}", background=True)
    await command.disconnect()
    equal(await state(command), "running", "a disconnect leaves the command running")

    async def exited() -> bool:
        return await state(command) == "exited"

    await wait_for("the disconnected command to end", 15, exited)
    equal(await sandbox.files.read_text(f"/tmp/{marker}"), f"{marker}\n")


async def cancel_keeps_remote(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    marker = ctx.name("cancel")
    await cancel_after(
        lambda on_stdout: sandbox.exec(f"echo started; sleep 3; echo {marker} > /tmp/{marker}", on_stdout=on_stdout),
        "started",
    )
    # A second client, as another process would make one, finds the command still running.
    other = await ctx.client().get(sandbox.id)
    entry = next((each for each in await other.commands.list() if marker in " ".join(each.command)), None)
    if entry is None:
        raise AssertionError("the second client lists the cancelled command")
    equal(entry.state, "running", "a client cancel leaves the remote command running")
    result = await (await other.commands.get(entry.id)).wait()
    equal(result.exit_code, 0)
    equal(result.stdout, "started\n")
    equal(await other.files.read_text(f"/tmp/{marker}"), f"{marker}\n")


CHECKS = [
    Check("commands.argv", argv),
    Check("commands.shell", shell),
    Check("commands.env_workdir_user", env_workdir_user),
    Check("commands.stdin", stdin),
    Check("commands.tty_resize", tty_resize),
    Check("commands.nonzero_result", nonzero_result),
    Check("commands.not_started_named", not_started_named),
    Check("commands.background_wait_kill", background_wait_kill),
    Check("commands.list_get", list_get),
    Check("commands.reconnect", reconnect),
    Check("commands.disconnect", disconnect),
    Check("commands.cancel_keeps_remote", cancel_keeps_remote),
]
