from __future__ import annotations

from useshards import ConflictError, NotFoundError

from .._shared import equal, named, ok
from .harness import AsyncContext, Check, rejects


async def needs_stopped(ctx: AsyncContext) -> None:
    running = await ctx.sandbox()
    await rejects(ConflictError, lambda: ctx.shard.snapshots.create(running, name=ctx.name("snapshot")))


async def create_list_inspect(ctx: AsyncContext) -> None:
    source = await ctx.snapshot_source()
    snapshot = await ctx.snapshot(source)
    equal(snapshot.source, source.id)
    name = named(snapshot.name, "the snapshot")
    ok(name.startswith(ctx.prefix), f"snapshot name {name}")
    ok(any(each.id == snapshot.id for each in await ctx.shard.snapshots.list()), "list holds the snapshot")
    equal((await ctx.shard.snapshots.inspect(snapshot.id)).name, snapshot.name)
    equal((await ctx.shard.snapshots.inspect(name)).id, snapshot.id)


async def create_from(ctx: AsyncContext) -> None:
    snapshot = await ctx.snapshot(await ctx.snapshot_source())
    sandbox = await ctx.create(snapshot=snapshot.id)
    equal(sandbox.info.state, "running")
    equal((await sandbox.exec("cat /root/marker")).stdout, "snapshot\n", "the new sandbox holds the snapshot's files")


async def outlive_source(ctx: AsyncContext) -> None:
    source = await ctx.stopped_source()
    snapshot = await ctx.snapshot(source)
    await source.remove()
    await rejects(NotFoundError, source.inspect)
    equal((await ctx.shard.snapshots.inspect(snapshot.id)).id, snapshot.id, "the snapshot outlives its source")
    sandbox = await ctx.create(snapshot=snapshot.id)
    equal((await sandbox.exec("cat /root/marker")).stdout, "snapshot\n")


async def remove(ctx: AsyncContext) -> None:
    snapshot = await ctx.snapshot(await ctx.snapshot_source())
    await ctx.shard.snapshots.remove(snapshot.id)
    await rejects(NotFoundError, lambda: ctx.shard.snapshots.inspect(snapshot.id))
    ok(all(each.id != snapshot.id for each in await ctx.shard.snapshots.list()), "list drops the snapshot")


CHECKS = [
    Check("snapshots.needs_stopped", needs_stopped),
    Check("snapshots.create_list_inspect", create_list_inspect),
    Check("snapshots.create_from", create_from),
    Check("snapshots.outlive_source", outlive_source),
    Check("snapshots.remove", remove),
]
