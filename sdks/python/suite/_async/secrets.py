from __future__ import annotations

import os

from useshards import ConflictError, NotFoundError

from .._shared import equal, ok
from .harness import AsyncContext, Check, rejects

DESTINATIONS = ("api.example.com",)


def value() -> str:
    return f"suite-value-{os.urandom(8).hex()}"


async def set_list_remove(ctx: AsyncContext) -> None:
    name = ctx.variable("secret")
    secret = value()
    await ctx.secret(name, secret, DESTINATIONS)
    listed = await ctx.shard.secrets.list()
    entry = next((each for each in listed.secrets if each.name == name), None)
    if entry is None:
        raise AssertionError("list holds the secret")
    equal(entry.destinations, DESTINATIONS)
    equal(entry.placeholder, f"mock-{name}")
    ok(entry.updated_at.tzinfo is not None, f"updated_at {entry.updated_at} carries its zone")
    ok(secret not in repr(listed), "a list never carries a value")
    await ctx.shard.secrets.set(name, value=f"{secret}-rotated", destinations=DESTINATIONS)
    rotated = next((each for each in (await ctx.shard.secrets.list()).secrets if each.name == name), None)
    equal(rotated.placeholder if rotated is not None else None, f"mock-{name}", "a rotation keeps the placeholder")
    await ctx.shard.secrets.remove(name)
    ok(all(each.name != name for each in (await ctx.shard.secrets.list()).secrets), "list drops the secret")
    await rejects(NotFoundError, lambda: ctx.shard.secrets.remove(name))


async def grant_ungrant(ctx: AsyncContext) -> None:
    name = ctx.variable("grant")
    secret = value()
    await ctx.secret(name, secret, DESTINATIONS)
    sandbox = await ctx.create()
    await rejects(ConflictError, lambda: ctx.shard.secrets.grant(sandbox, name))
    await sandbox.stop()
    await ctx.shard.secrets.grant(sandbox, name)
    equal((await sandbox.inspect()).secrets, (name,))
    await sandbox.start()
    printed = await sandbox.exec(f"printenv {name}")
    equal(printed.stdout, f"mock-{name}\n", "the guest holds the placeholder")
    ok(secret not in printed.stdout, "the guest never holds the value")
    await rejects(ConflictError, lambda: ctx.shard.secrets.remove(name))
    await sandbox.stop()
    await ctx.shard.secrets.ungrant(sandbox, name)
    equal((await sandbox.inspect()).secrets, ())
    await sandbox.start()
    equal((await sandbox.exec(f"printenv {name}")).exit_code, 1, "an ungrant takes the placeholder back")
    holder = await ctx.create(secrets=[name])
    equal((await holder.exec(f"printenv {name}")).stdout, f"mock-{name}\n", "a create grants it too")


CHECKS = [
    Check("secrets.set_list_remove", set_list_remove),
    Check("secrets.grant_ungrant", grant_ungrant),
]
