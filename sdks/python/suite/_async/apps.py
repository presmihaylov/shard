from __future__ import annotations

from useshards import ExitStatus, Restart

from .._shared import equal, ok
from .harness import AsyncContext, Check
from .mode import sleep


async def run_handle(ctx: AsyncContext) -> None:
    command = ["sh", "-c", "echo up; sleep 300"]
    app = await ctx.run(command)
    ok(len(app.sandbox.id) > 0, "the handle holds its sandbox")
    equal(app.sandbox.info.state, "running")
    info = await app.inspect()
    equal(info.command, tuple(command))
    equal(info.exit_status, None, "a running app has no exit yet")
    equal(info.restart, None, "run with no restart option sets no policy")
    record = (await app.sandbox.inspect()).app
    equal(record.command if record is not None else None, tuple(command), "the sandbox record names its app")


async def sandbox_exec(ctx: AsyncContext) -> None:
    app = await ctx.run(["sleep", "300"])
    result = await app.sandbox.exec(["ps", "-o", "args"])
    equal(result.exit_code, 0, result.stderr)
    ok("sleep 300" in result.stdout.split("\n"), "a command in the app's sandbox sees the app")


async def inspect_logs_wait(ctx: AsyncContext) -> None:
    app = await ctx.run("echo out; echo err >&2; exit 4")
    exit_ = await app.wait()
    equal(exit_.exit_code, 4)
    equal(exit_.signal, None)
    equal(exit_.restarts, 0)
    equal((await app.inspect()).exit_status, ExitStatus(code=4, signal=None))
    logs = await app.logs()
    ok("out\n" in logs and "err\n" in logs, f"the logs hold both streams: {logs!r}")
    equal((await app.sandbox.inspect()).state, "running", "the sandbox outlives its app")
    equal((await app.sandbox.exec("echo after")).stdout, "after\n")


async def stop_keeps_sandbox(ctx: AsyncContext) -> None:
    # always would start a TERMed app again, so the stop must cancel the policy, not only signal the app.
    app = await ctx.run(["sleep", "300"], restart=Restart(policy="always", backoff=1))
    await app.stop()
    exit_ = await app.wait()
    equal(exit_.signal, 15, "stop ends the app with TERM")
    equal(exit_.restarts, 0)
    await sleep(3)
    restart = (await app.inspect()).restart
    if restart is None:
        raise AssertionError("the app reports its policy")
    equal(restart.count, 0, "no start again follows the stop, past the backoff")
    equal(restart.ended, True)
    equal((await app.sandbox.inspect()).state, "running", "stop ends the app and keeps the sandbox")
    ps = await app.sandbox.exec(["ps", "-o", "args"])
    ok("sleep 300" not in ps.stdout.split("\n"), f"the app is gone: {ps.stdout!r}")


async def restart_policy(ctx: AsyncContext) -> None:
    app = await ctx.run("echo start; exit 1", restart=Restart(policy="on-failure", retries=2, backoff=1))
    exit_ = await app.wait()
    equal(exit_.exit_code, 1)
    equal(exit_.restarts, 2, "wait answers once the retries are spent")
    restart = (await app.inspect()).restart
    if restart is None:
        raise AssertionError("the app reports its policy")
    equal(restart.policy, "on-failure")
    equal(restart.retries, 2)
    equal(restart.count, 2)
    equal(restart.gave_up, True)
    equal(restart.ended, True)
    starts = [line for line in (await app.logs()).split("\n") if line == "start"]
    equal(len(starts), 3, "the first start and two more")


CHECKS = [
    Check("apps.run_handle", run_handle),
    Check("apps.sandbox_exec", sandbox_exec),
    Check("apps.inspect_logs_wait", inspect_logs_wait),
    Check("apps.stop_keeps_sandbox", stop_keeps_sandbox),
    Check("apps.restart_policy", restart_policy),
]
