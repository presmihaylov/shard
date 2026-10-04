"""What the async client runs on: asyncio. The sync client's twin of this file runs on threads."""

from __future__ import annotations

import asyncio
from collections.abc import Callable, Coroutine
from typing import Any, TypeVar

T = TypeVar("T")

Lock = asyncio.Lock
Event = asyncio.Event
sleep = asyncio.sleep


async def offload(fn: Callable[[], T]) -> T:
    """Run blocking disk work off the loop."""
    return await asyncio.to_thread(fn)


async def wait_event(event: asyncio.Event, timeout: float | None) -> bool:
    if timeout is None:
        await event.wait()
        return True
    try:
        await asyncio.wait_for(event.wait(), timeout)
    except TimeoutError:
        return False
    return True


class Task:
    """One background job, as a task on the running loop."""

    def __init__(self, fn: Callable[[], Coroutine[Any, Any, None]]) -> None:
        self._task = asyncio.get_running_loop().create_task(fn())

    async def join(self, timeout: float | None) -> bool:
        done, _ = await asyncio.wait({self._task}, timeout=timeout)
        return bool(done)

    async def cancel(self) -> None:
        self._task.cancel()
        await asyncio.wait({self._task})
