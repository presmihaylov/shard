"""The Context every check gets: the run it shares, and the resources it must undo when it ends."""

from __future__ import annotations

import time
from collections.abc import Mapping, Sequence
from secrets import token_hex
from typing import TypeVar

import attrs

from useshards import (
    AsyncSandbox,
    AsyncShard,
    NotFoundError,
    Policy,
    PolicyRule,
    Snapshot,
)

from .._shared import describe, image
from .mode import Body, Call, Probe, sleep

E = TypeVar("E", bound=BaseException)


@attrs.define
class _Run:
    """What every check of one run shares: the client, the image, the names and the fixtures."""

    shard: AsyncShard
    image: str
    # Both SDK suites may share one daemon, so every name carries the language and a run id.
    prefix: str
    count: int = 0
    fixtures: dict[str, AsyncSandbox] = attrs.Factory(dict)


@attrs.frozen
class Check:
    name: str
    run: Body[AsyncContext]


@attrs.frozen
class _Undo:
    what: str
    call: Call


class AsyncContext:
    def __init__(self, run: _Run, root: AsyncContext | None) -> None:
        self._run = run
        # The suite's own Context owns the fixtures and undoes them after the last check.
        self._root = root
        self._undo: list[_Undo] = []

    @classmethod
    def suite(cls) -> AsyncContext:
        return cls(_Run(shard=AsyncShard(), image=image(), prefix=f"suite-py-{token_hex(3)}"), None)

    def check(self) -> AsyncContext:
        """The Context of one check, so its cleanup leaves nothing behind for the next one."""
        return AsyncContext(self._run, self._root or self)

    @property
    def shard(self) -> AsyncShard:
        return self._run.shard

    @property
    def image(self) -> str:
        return self._run.image

    @property
    def prefix(self) -> str:
        return self._run.prefix

    def name(self, kind: str) -> str:
        self._run.count += 1
        return f"{self._run.prefix}-{kind}-{self._run.count}"

    def variable(self, kind: str) -> str:
        """A unique name shaped like an environment variable, which a secret name must be."""
        return self.name(kind).upper().replace("-", "_")

    def client(self) -> AsyncShard:
        """A second, independent client, as another process would make one."""
        shard = AsyncShard()
        self.defer("close the second client", shard.aclose)
        return shard

    async def create(
        self,
        *,
        snapshot: str | None = None,
        env: Mapping[str, str] | None = None,
        memory_mib: int | None = None,
        policy: str | None = None,
        secrets: Sequence[str] | None = None,
    ) -> AsyncSandbox:
        sandbox = await self.shard.create(
            None if snapshot is not None else self.image,
            name=self.name("sandbox"),
            snapshot=snapshot,
            env=env,
            memory_mib=memory_mib,
            policy=policy,
            secrets=secrets,
        )
        self.defer(f"remove sandbox {sandbox.id}", lambda: sandbox.remove(force=True))
        return sandbox

    async def policy(self, name: str, rules: Sequence[PolicyRule]) -> Policy:
        """Set one; cleanup runs in reverse, so a sandbox made after it is gone before the policy goes."""
        policy = await self.shard.policies.set(name, rules)
        self.defer(f"remove policy {name}", lambda: self.shard.policies.remove(name))
        return policy

    async def secret(self, name: str, value: str, destinations: Sequence[str]) -> None:
        await self.shard.secrets.set(name, value=value, destinations=destinations)
        self.defer(f"remove secret {name}", lambda: self.shard.secrets.remove(name, force=True))

    async def snapshot(self, source: AsyncSandbox) -> Snapshot:
        snapshot = await self.shard.snapshots.create(source, name=self.name("snapshot"))
        self.defer(f"remove snapshot {snapshot.id}", lambda: self.shard.snapshots.remove(snapshot.id))
        return snapshot

    async def stopped_source(self) -> AsyncSandbox:
        """A stopped sandbox with a marker file, the one state a snapshot takes."""
        source = await self.create()
        await source.exec("echo snapshot > /root/marker")
        await source.stop()
        return source

    async def sandbox(self) -> AsyncSandbox:
        """One running sandbox that checks which change nothing lasting share."""
        held = self._run.fixtures.get("sandbox")
        if held is not None:
            return held
        made = await self._owner().create()
        self._run.fixtures["sandbox"] = made
        return made

    async def snapshot_source(self) -> AsyncSandbox:
        held = self._run.fixtures.get("snapshot-source")
        if held is not None:
            return held
        made = await self._owner().stopped_source()
        self._run.fixtures["snapshot-source"] = made
        return made

    def _owner(self) -> AsyncContext:
        """A fixture is made under the suite's Context, so it outlives the check that made it."""
        return self._root or self

    def defer(self, what: str, call: Call) -> None:
        self._undo.append(_Undo(what=what, call=call))

    async def cleanup(self) -> list[str]:
        """Undo everything in reverse and answer what failed; a resource already gone is not a failure."""
        failed = []
        undo, self._undo = self._undo, []
        for each in reversed(undo):
            try:
                await each.call()
            except NotFoundError:
                continue
            except Exception as e:
                failed.append(f"{each.what}: {describe(e)}")
        return failed

    async def aclose(self) -> None:
        await self.shard.aclose()


async def rejects(kind: type[E], call: Call) -> E:
    try:
        await call()
    except kind as e:
        return e
    except Exception as e:
        raise AssertionError(f"want {kind.__name__}, got {describe(e)}") from e
    raise AssertionError(f"want {kind.__name__}, got no error")


async def wait_for(what: str, seconds: float, probe: Probe) -> None:
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if await probe():
            return
        await sleep(0.2)
    raise AssertionError(f"{what} did not happen within {seconds:g} s")
