from __future__ import annotations

from useshards import AsyncSandbox, EgressDecision, PolicyRule

from .._shared import equal, matches, not_equal, ok
from .harness import AsyncContext, Check, wait_for
from .mode import Cancelled, consume


async def denied(ctx: AsyncContext) -> AsyncSandbox:
    """A running sandbox under a policy that denies everything, so any request it makes leaves a deny record."""
    policy = ctx.name("deny")
    await ctx.policy(policy, [PolicyRule(action="deny", rule="any")])
    return await ctx.create(policy=policy)


async def request(sandbox: AsyncSandbox, host: str) -> None:
    result = await sandbox.exec(f"wget -q -T 5 -O /dev/null http://{host}/")
    not_equal(result.exit_code, 0, f"the policy let {host} through")


def denied_host(records: list[EgressDecision], host: str) -> EgressDecision | None:
    return next((record for record in records if record.verdict == "deny" and record.host == host), None)


async def read(ctx: AsyncContext) -> None:
    sandbox = await ctx.create()
    job = await sandbox.run("echo line-one; echo line-two >&2; sleep 300")
    logs = ""

    async def both() -> bool:
        nonlocal logs
        logs = await job.logs()
        return "line-two\n" in logs

    await wait_for("both lines in the logs", 15, both)
    ok("line-one\n" in logs, f"the logs hold stdout: {logs!r}")


async def follow(ctx: AsyncContext) -> None:
    sandbox = await ctx.create()
    job = await sandbox.run("for i in 1 2 3; do echo tick $i; sleep 1; done; sleep 300")
    out = b""
    async with job.follow_logs() as stream:
        async for chunk in stream:
            out += chunk
            if b"tick 3\n" in out:
                break
    matches(out.decode(), r"tick 1\n[\s\S]*tick 2\n[\s\S]*tick 3\n", "the follow yields the lines as they come")

    rest = bytearray()
    ended = consume(job.follow_logs(), rest.extend)

    async def replayed() -> bool:
        return b"tick 3\n" in rest

    await wait_for("the second follow to replay the log", 10, replayed)
    ok(rest.startswith(b"tick 1\n"), "a follow starts at the beginning of the log")
    await sandbox.stop()
    equal(await ended.result(), None, "a stop ends the follow without an error")


async def egress_read(ctx: AsyncContext) -> None:
    sandbox = await denied(ctx)
    host = f"{ctx.name('host')}.example"
    await request(sandbox, host)
    records: list[EgressDecision] = []

    async def recorded() -> bool:
        nonlocal records
        records = await sandbox.egress_log()
        return denied_host(records, host) is not None

    await wait_for(f"a deny record for {host}", 10, recorded)
    record = denied_host(records, host)
    if record is None:
        raise AssertionError(f"no deny record for {host}")
    ok(record.time.tzinfo is not None, f"time {record.time} carries its zone")
    ok(len(record.rule) > 0, "a record names the rule that decided it")


async def egress_follow(ctx: AsyncContext) -> None:
    sandbox = await denied(ctx)
    host = f"{ctx.name('host')}.example"
    seen: list[EgressDecision] = []
    ended = consume(sandbox.follow_egress_log(), seen.append)
    try:
        await request(sandbox, host)

        async def recorded() -> bool:
            return denied_host(seen, host) is not None

        await wait_for(f"the follow to yield a deny record for {host}", 10, recorded)
    finally:
        ended.cancel()
    ok(isinstance(await ended.result(), Cancelled), "a cancel ends the follow and nothing else does")


CHECKS = [
    Check("logs.read", read),
    Check("logs.follow", follow),
    Check("logs.egress_read", egress_read),
    Check("logs.egress_follow", egress_follow),
]
