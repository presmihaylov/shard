from __future__ import annotations

from useshards import ConflictError, ExitStatus, NotFoundError, Restart

from .._shared import equal, ok
from .harness import AsyncContext, Check, rejects
from .mode import sleep


async def run_handle(ctx: AsyncContext) -> None:
    sandbox = await ctx.create()
    command = ["sh", "-c", "echo up; sleep 300"]
    web = await sandbox.run(command, name="web")
    equal(web.name, "web")
    equal(web.info.command, tuple(command))
    equal(web.info.status.state, "running")
    equal(web.info.status.exit, None, "a running process has no exit yet")
    equal(web.info.restart.policy, "unless-stopped", "a run with no restart option is unless-stopped")
    names = [each.name for each in (await sandbox.inspect()).processes]
    equal(names, ["web"], "the sandbox record lists its process")
    equal((await sandbox.run(["sleep", "300"])).name, "sleep", "a run with no name takes its program's base name")


async def list_get(ctx: AsyncContext) -> None:
    sandbox = await ctx.create()
    await sandbox.run(["sleep", "300"], name="first")
    await sandbox.run(["sleep", "301"], name="second")
    equal([each.name for each in await sandbox.processes.list()], ["first", "second"], "list answers in run order")
    equal((await sandbox.processes.get("second")).info.status.state, "running")
    ps = await sandbox.exec(["ps", "-o", "args"])
    ok("sleep 301" in ps.stdout.split("\n"), "a command in the sandbox sees its processes")
    equal((await rejects(NotFoundError, lambda: sandbox.processes.get("none"))).code, "no_process")
    taken = await rejects(ConflictError, lambda: sandbox.run(["sleep", "300"], name="first"))
    equal(taken.code, "name_taken")


async def wait_logs(ctx: AsyncContext) -> None:
    sandbox = await ctx.create()
    job = await sandbox.run("echo out; echo err >&2; exit 4", restart=Restart(policy="no"))
    ended = await job.wait()
    equal(ended.status.state, "exited")
    equal(ended.status.exit, ExitStatus(code=4, signal=None))
    logs = await job.logs()
    ok("out\n" in logs and "err\n" in logs, f"the logs hold both streams: {logs!r}")
    equal((await sandbox.inspect()).state, "running", "the sandbox outlives its process")
    equal((await sandbox.exec("echo after")).stdout, "after\n")


async def kill_keeps_sandbox(ctx: AsyncContext) -> None:
    sandbox = await ctx.create()
    # always would start a TERMed process again, so the kill must cancel the policy, not only signal the process.
    web = await sandbox.run(["sleep", "300"], restart=Restart(policy="always", backoff=1))
    killed = await web.kill()
    equal(killed.status.state, "killed")
    equal(killed.status.exit.signal if killed.status.exit is not None else None, 15, "kill ends the process with TERM")
    equal(killed.killed, True)
    await sleep(3)
    after = await web.inspect()
    equal(after.status.state, "killed", "no start again follows the kill, past the backoff")
    equal(after.status.restarts, 0)
    equal((await sandbox.inspect()).state, "running", "kill ends the process and keeps the sandbox")
    ps = await sandbox.exec(["ps", "-o", "args"])
    ok("sleep 300" not in ps.stdout.split("\n"), f"the process is gone: {ps.stdout!r}")


async def restart_policy(ctx: AsyncContext) -> None:
    sandbox = await ctx.create()
    job = await sandbox.run("echo start; exit 1", restart=Restart(policy="on-failure", retries=2, backoff=1))
    ended = await job.wait()
    equal(ended.status.state, "gave-up", "wait answers once the retries are spent")
    equal(ended.status.exit.code if ended.status.exit is not None else None, 1)
    equal(ended.status.restarts, 2)
    equal(ended.restart, Restart(policy="on-failure", retries=2, backoff=1))
    starts = [line for line in (await job.logs()).split("\n") if line == "start"]
    equal(len(starts), 3, "the first start and two more")


async def run_again(ctx: AsyncContext) -> None:
    sandbox = await ctx.create()
    await (await sandbox.run("echo ran", name="job", restart=Restart(policy="no"))).wait()
    again = await sandbox.run("echo ran", name="job", restart=Restart(policy="no"))
    await again.wait()
    runs = [line for line in (await again.logs()).split("\n") if line == "ran"]
    equal(len(runs), 2, "a run of an ended name appends to its log")
    equal(len(await sandbox.processes.list()), 1, "the run takes the ended entry's place")


CHECKS = [
    Check("processes.run_handle", run_handle),
    Check("processes.list_get", list_get),
    Check("processes.wait_logs", wait_logs),
    Check("processes.kill_keeps_sandbox", kill_keeps_sandbox),
    Check("processes.restart_policy", restart_policy),
    Check("processes.run_again", run_again),
]
