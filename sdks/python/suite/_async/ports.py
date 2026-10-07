from __future__ import annotations

from useshards import ConflictError, NotFoundError, PortForward

from .._shared import equal, free_port, ok, on_this_host, skip
from .harness import AsyncContext, Check, rejects, wait_for
from .mode import greeting

GUEST_PORT = 8000


async def add_connect_remove(ctx: AsyncContext) -> None:
    if not on_this_host():
        raise skip("SHARD_REMOTE names another host, and a private forward listens on that host's 127.0.0.1 alone")
    sandbox = await ctx.create()
    marker = ctx.name("port")
    # A loop, since busybox nc answers one connection and a probe of the guest port may take it.
    listener = await sandbox.exec(f"while true; do echo {marker} | nc -l -p {GUEST_PORT}; done", background=True)
    await listener.disconnect()
    host_port = free_port()
    added = await sandbox.ports.add(host_port, GUEST_PORT)
    equal(added.sandbox, sandbox.id)
    equal((added.host_port, added.guest_port, added.public, added.address), (host_port, GUEST_PORT, False, "127.0.0.1"))
    ok(added.listening, f"a running sandbox listens at once: {added.error}")
    reachable = [on.address for on in added.reachable_on]
    ok(
        bool(reachable) and all(address == "127.0.0.1" for address in reachable),
        f"a private forward is on loopback: {reachable}",
    )

    async def answers() -> bool:
        try:
            return await greeting(host_port) == f"{marker}\n".encode()
        except OSError:
            return False

    await wait_for("the guest line through the host port", 30, answers)
    listed = [(each.host_port, each.guest_port, each.listening) for each in await sandbox.ports.list()]
    equal(listed, [(host_port, GUEST_PORT, True)])
    every = await ctx.shard.ports()
    ok(
        any(each.host_port == host_port and each.sandbox == sandbox.id for each in every),
        "the list of every forward holds it",
    )
    equal((await sandbox.inspect()).ports, (PortForward(host_port=host_port, guest_port=GUEST_PORT),))
    await sandbox.ports.remove(host_port)
    equal(await sandbox.ports.list(), [])
    await rejects(ConnectionRefusedError, lambda: greeting(host_port))


async def held_by_one_sandbox(ctx: AsyncContext) -> None:
    owner = await ctx.create()
    await owner.stop()
    # A stopped sandbox binds nothing, so any host port works, on this host or another.
    host_port = free_port()
    added = await owner.ports.add(host_port, GUEST_PORT, public=True)
    equal((added.public, added.address, added.listening, added.reachable_on), (True, "0.0.0.0", False, ()))
    equal((await owner.inspect()).ports, (PortForward(host_port=host_port, guest_port=GUEST_PORT, public=True),))
    other = await ctx.sandbox()
    await rejects(ConflictError, lambda: other.ports.add(host_port, GUEST_PORT))
    await rejects(NotFoundError, lambda: other.ports.remove(host_port))
    await owner.ports.remove(host_port)
    equal((await owner.inspect()).ports, ())
    await rejects(NotFoundError, lambda: owner.ports.remove(host_port))


CHECKS = [
    Check("ports.add_connect_remove", add_connect_remove),
    Check("ports.held_by_one_sandbox", held_by_one_sandbox),
]
