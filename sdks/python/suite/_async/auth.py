from __future__ import annotations

import json
import os

from useshards import AsyncSandbox, AsyncShard, AuthenticationError, ConfigurationError

from .._shared import env, equal, ok, raw
from .harness import AsyncContext, Check, rejects

# The local routes 488 keeps off serve; a probe that would change the daemon goes last, after every other refusal.
LOCAL_PROBES = [
    ("GET", "/v0/daemon"),
    ("GET", "/v0/local/sandboxes"),
    ("GET", "/v0/local/sandboxes/suite-py-no-such-sandbox"),
    ("GET", "/v0/images"),
    ("DELETE", "/v0/images/suite-py-no-such-image:none"),
    ("POST", "/v0/images/pull"),
    ("POST", "/v0/images/prune"),
]

# A scoped key is refused a local route on main too, so only a "*" key proves serve keeps the route local.
WILDCARD_KEY_ENV = "SHARD_SUITE_WILDCARD_KEY"


async def listed(remote: str | None = None, api_key: str | None = None) -> list[AsyncSandbox]:
    async with AsyncShard(remote=remote, api_key=api_key) as shard:
        return await shard.list()


async def env_defaults(ctx: AsyncContext) -> None:
    ok(isinstance(await listed(), list), "a client from SHARD_REMOTE and SHARD_API_KEY lists sandboxes")


async def explicit_overrides(ctx: AsyncContext) -> None:
    remote = os.environ.get("SHARD_REMOTE")
    api_key = os.environ.get("SHARD_API_KEY")
    with env(SHARD_REMOTE="https://shard.invalid", SHARD_API_KEY="shard_not_a_key"):
        ok(isinstance(await listed(remote, api_key), list), "explicit settings win over bad env")
    await rejects(AuthenticationError, lambda: listed(api_key="shard_not_a_key"))


async def no_key(ctx: AsyncContext) -> None:
    with env(SHARD_API_KEY=None):
        await rejects(ConfigurationError, listed)


async def bad_key(ctx: AsyncContext) -> None:
    err = await rejects(AuthenticationError, lambda: listed(api_key="shard_not_a_key"))
    equal(err.status, 401)


async def local_routes_refused(ctx: AsyncContext) -> None:
    wildcard = os.environ.get(WILDCARD_KEY_ENV)
    if not wildcard:
        raise AssertionError(f'{WILDCARD_KEY_ENV} is not set: it must hold a "*" token')
    keys = [("SHARD_API_KEY", os.environ.get("SHARD_API_KEY", "")), (WILDCARD_KEY_ENV, wildcard)]
    refusal: str | None = None
    for method, path in LOCAL_PROBES:
        for name, key in keys:
            got = raw(method, path, key)
            equal(got.status, 403, f"{method} {path} with {name} answered {got.body!r}")
            # Every local route reads like an unknown one, so the body is the same for all of them.
            refusal = got.body if refusal is None else refusal
            equal(got.body, refusal, f"{method} {path} with {name} answered a different refusal")
    unknown = raw("GET", "/v0/suite-py-no-such-route")
    equal(unknown.status, 403)
    equal(unknown.body, refusal, "a local route is refused like an unknown route")


async def version_public(ctx: AsyncContext) -> None:
    version = await ctx.shard.version()
    ok(len(version.version) > 0, "version names the daemon version")
    got = raw("GET", "/v0/version")
    equal(got.status, 200)
    keys = sorted(set(json.loads(got.body)) - {"version", "api_version"})
    equal(keys, [], "the public version holds nothing but versions")


CHECKS = [
    Check("auth.env_defaults", env_defaults),
    Check("auth.explicit_overrides", explicit_overrides),
    Check("auth.no_key", no_key),
    Check("auth.bad_key", bad_key),
    Check("auth.local_routes_refused", local_routes_refused),
    Check("auth.version_public", version_public),
]
