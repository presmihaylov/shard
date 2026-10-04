from __future__ import annotations

import re

import attrs

from useshards import AsyncSandbox, ConflictError, NotFoundError, UnsupportedError

from .._shared import equal, matches, named, not_equal, ok, skip
from .harness import AsyncContext, Check, rejects
from .mode import Call

INIT = re.compile(r"^(\S*/)?(shard-)?init\b")


async def others(sandbox: AsyncSandbox) -> list[str]:
    """The guest processes besides init and the ps that lists them."""
    result = await sandbox.exec(["ps", "-o", "args"])
    equal(result.exit_code, 0, result.stderr)
    # Kernel threads print in brackets on a microVM, and a container has none.
    return [
        line
        for line in result.stdout.split("\n")[1:]
        if line and not line.startswith("[") and line != "ps -o args" and not INIT.match(line)
    ]


async def create_no_app(ctx: AsyncContext) -> None:
    sandbox = await ctx.create(env={"MODE": "test"}, memory_mib=256)
    equal(sandbox.info.state, "running")
    equal(sandbox.info.resources.memory_mib, 256)
    equal(sandbox.info.app, None, "create starts no app")
    equal((await sandbox.exec("printenv MODE")).stdout, "test\n")
    equal(await others(sandbox), [], "only init and the exec itself run in a sandbox with no app")


async def inspect_get_list(ctx: AsyncContext) -> None:
    sandbox = await ctx.sandbox()
    info = await sandbox.inspect()
    equal(info.id, sandbox.id)
    equal(info.state, "running")
    ok(info.image.endswith(ctx.image), f"image {info.image}")
    equal((await ctx.shard.get(sandbox.id)).id, sandbox.id)
    equal((await ctx.shard.get(named(sandbox.name, "the sandbox"))).id, sandbox.id)
    ok(any(each.id == sandbox.id for each in (await ctx.shard.list()).sandboxes), "list holds the sandbox")
    await rejects(NotFoundError, lambda: ctx.shard.get(f"{ctx.prefix}-no-such-sandbox"))


async def stop_start(ctx: AsyncContext) -> None:
    sandbox = await ctx.create()
    await sandbox.exec("echo kept > /root/kept")
    await sandbox.stop()
    equal((await sandbox.inspect()).state, "stopped")
    await rejects(ConflictError, lambda: sandbox.exec("true"))
    await sandbox.start()
    equal((await sandbox.inspect()).state, "running")
    equal((await sandbox.exec("cat /root/kept")).stdout, "kept\n", "a start keeps the root a stop left")


async def pause_resume(ctx: AsyncContext) -> None:
    sandbox = await ctx.create()
    await sandbox.exec("echo before > /root/before")
    try:
        await sandbox.pause()
    except UnsupportedError as e:
        raise skip(f"the provider has no pause: {e.message}") from None
    equal((await sandbox.inspect()).state, "paused")
    await rejects(ConflictError, lambda: sandbox.exec("true"))
    await sandbox.resume()
    equal((await sandbox.inspect()).state, "running")
    equal((await sandbox.exec("cat /root/before")).stdout, "before\n")


async def fork(ctx: AsyncContext) -> None:
    source = await ctx.create()
    await source.exec("echo source > /root/origin")
    try:
        forked = await source.fork(name=ctx.name("fork"))
    except UnsupportedError as e:
        raise skip(f"the provider has no fork: {e.message}") from None
    ctx.defer(f"remove sandbox {forked.id}", lambda: forked.remove(force=True))
    not_equal(forked.id, source.id)
    equal((await forked.inspect()).state, "running")
    equal((await forked.exec("cat /root/origin")).stdout, "source\n", "the fork holds the source's files")
    await forked.exec("echo fork > /root/origin")
    equal((await source.exec("cat /root/origin")).stdout, "source\n", "a write in the fork leaves the source")
    equal((await source.inspect()).state, "running", "the source runs on")


async def remove(ctx: AsyncContext) -> None:
    running = await ctx.create()
    await rejects(ConflictError, lambda: running.remove())
    await running.remove(force=True)
    await rejects(NotFoundError, lambda: ctx.shard.get(running.id))
    stopped = await ctx.create()
    await stopped.stop()
    await stopped.remove()
    await rejects(NotFoundError, stopped.inspect)


async def unsupported_named(ctx: AsyncContext) -> None:
    sandbox = await ctx.create()

    async def pause_then_resume() -> None:
        await sandbox.pause()
        await sandbox.resume()

    async def fork_then_remove() -> None:
        forked = await sandbox.fork(name=ctx.name("fork"))
        await forked.remove(force=True)

    attempts: list[tuple[str, Call]] = [("pause", pause_then_resume), ("fork", fork_then_remove)]
    refused: list[UnsupportedError] = []
    for verb, attempt in attempts:
        try:
            await attempt()
        except UnsupportedError as e:
            matches(e.message, verb, f"the refusal names {verb}")
            refused.append(e)
    if not refused:
        raise skip("the provider runs every optional verb")
    for err in refused:
        equal(err.code, "unsupported")


async def agrees(verb: str, supported: bool, run: Call) -> None:
    """Run a verb, and prove the capabilities said whether the server would refuse it."""
    try:
        await run()
    except UnsupportedError as e:
        equal(supported, False, f"capabilities say {verb}, but the server refused it: {e.message}")
        matches(e.message, verb, f"the refusal names {verb}")
        return
    equal(supported, True, f"capabilities say no {verb}, but it ran")


async def capabilities(ctx: AsyncContext) -> None:
    verbs = attrs.asdict(await ctx.shard.capabilities())
    equal(sorted(verbs), ["create", "fork", "pause", "remove", "resume", "snapshot", "start", "stop"])
    ok(all(isinstance(value, bool) for value in verbs.values()), f"every verb is a boolean: {verbs}")
    for verb in ("create", "start", "stop", "remove"):
        equal(verbs[verb], True, f"every provider can {verb}")
    equal(verbs["resume"], verbs["pause"], "pause and resume come as a pair")
    sandbox = await ctx.create()

    async def pause_then_resume() -> None:
        await sandbox.pause()
        await sandbox.resume()

    async def fork_then_remove() -> None:
        forked = await sandbox.fork(name=ctx.name("fork"))
        await forked.remove(force=True)

    async def snapshot() -> None:
        await ctx.snapshot(sandbox)

    await agrees("pause", verbs["pause"], pause_then_resume)
    await agrees("fork", verbs["fork"], fork_then_remove)
    await sandbox.stop()
    await agrees("snapshot", verbs["snapshot"], snapshot)


CHECKS = [
    Check("lifecycle.create_no_app", create_no_app),
    Check("lifecycle.inspect_get_list", inspect_get_list),
    Check("lifecycle.stop_start", stop_start),
    Check("lifecycle.pause_resume", pause_resume),
    Check("lifecycle.fork", fork),
    Check("lifecycle.remove", remove),
    Check("lifecycle.unsupported_named", unsupported_named),
    Check("lifecycle.capabilities", capabilities),
]
