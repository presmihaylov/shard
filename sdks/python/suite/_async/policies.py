from __future__ import annotations

from useshards import ConflictError, InvalidRequestError, NotFoundError, PolicyRule

from .._shared import equal, ok
from .harness import AsyncContext, Check, rejects

# Each rule is spelled as the daemon spells it back, so a round trip compares equal.
RULES = (
    PolicyRule(action="allow", rule="suffix:example.com tcp:443"),
    PolicyRule(action="allow", rule="10.0.0.0/8 tcp:22"),
    PolicyRule(action="deny", rule="any"),
)


async def set_get_list_remove(ctx: AsyncContext) -> None:
    name = ctx.name("policy")
    made = await ctx.policy(name, RULES)
    equal(made.name, name)
    equal(made.rules, RULES)
    equal((await ctx.shard.policies.get(name)).rules, RULES, "the rules come back in order")
    ok(any(each.name == name for each in await ctx.shard.policies.list()), "list holds the policy")
    replaced = (PolicyRule(action="deny", rule="any"),)
    await ctx.shard.policies.set(name, replaced)
    equal((await ctx.shard.policies.get(name)).rules, replaced, "a set replaces every rule")
    bad = [PolicyRule(action="allow", rule="not a rule at all")]
    await rejects(InvalidRequestError, lambda: ctx.shard.policies.set(ctx.name("bad"), bad))
    await ctx.shard.policies.remove(name)
    await rejects(NotFoundError, lambda: ctx.shard.policies.get(name))


async def attach_detach(ctx: AsyncContext) -> None:
    name = ctx.name("policy")
    await ctx.policy(name, RULES)
    sandbox = await ctx.create()
    equal(sandbox.info.policy, None)
    await rejects(ConflictError, lambda: ctx.shard.policies.attach(sandbox, name))
    await sandbox.stop()
    await ctx.shard.policies.attach(sandbox, name)
    equal((await sandbox.inspect()).policy, name)
    await rejects(ConflictError, lambda: ctx.shard.policies.remove(name))
    await ctx.shard.policies.detach(sandbox)
    equal((await sandbox.inspect()).policy, None)


CHECKS = [
    Check("policies.set_get_list_remove", set_get_list_remove),
    Check("policies.attach_detach", attach_detach),
]
