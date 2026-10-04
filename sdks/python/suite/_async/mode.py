"""What only the async mode does: the clock, the background read, and how a check cuts a call short."""

from __future__ import annotations

import asyncio
from collections.abc import AsyncIterator, Awaitable, Callable
from typing import Any, Protocol, TypeVar

from useshards import OutputCallback

T = TypeVar("T", covariant=True)
C = TypeVar("C")

Call = Callable[[], Awaitable[object]]
Probe = Callable[[], Awaitable[bool]]
Body = Callable[[C], Awaitable[None]]
Cancelled = asyncio.CancelledError

SEEN_WITHIN = 60.0


class Source(Protocol[T]):
    def __aiter__(self) -> AsyncIterator[T]: ...

    async def aclose(self) -> None: ...


async def sleep(seconds: float) -> None:
    await asyncio.sleep(seconds)


async def within(seconds: float, run: Call) -> None:
    scope = asyncio.timeout(seconds)
    try:
        async with scope:
            await run()
    except TimeoutError:
        if scope.expired():
            raise AssertionError(f"the check did not end within {seconds:g} s") from None
        raise


class Pending:
    """A read left running in the background, which answers what ended it so an error is never lost unseen."""

    def __init__(self, task: asyncio.Task[BaseException | None]) -> None:
        self._task = task

    async def result(self) -> BaseException | None:
        await asyncio.wait({self._task})
        if self._task.cancelled():
            return Cancelled()
        return self._task.result()

    def cancel(self) -> None:
        self._task.cancel()


def consume(source: Source[T], each: Callable[[T], None]) -> Pending:
    async def read() -> BaseException | None:
        try:
            async for item in source:
                each(item)
        except Exception as e:
            return e
        finally:
            await source.aclose()
        return None

    return Pending(asyncio.get_running_loop().create_task(read()))


class Printed:
    """An output callback that marks the point a command printed the given text."""

    def __init__(self, want: str) -> None:
        self._want = want.encode()
        self._out = b""
        self._seen = asyncio.Event()

    def on_stdout(self, chunk: bytes) -> None:
        self._out += chunk
        if self._want in self._out:
            self._seen.set()

    async def seen(self) -> None:
        try:
            await asyncio.wait_for(self._seen.wait(), SEEN_WITHIN)
        except TimeoutError:
            raise AssertionError(f"the command did not print {self._want!r} within {SEEN_WITHIN:g} s") from None


async def cancel_after(run: Callable[[OutputCallback], Awaitable[object]], marker: str) -> None:
    """Cancel the call once the command printed the marker, and fail unless the cancel is what ended it."""
    printed = Printed(marker)
    call = asyncio.ensure_future(run(printed.on_stdout))
    seen = asyncio.ensure_future(printed.seen())
    await asyncio.wait({call, seen}, return_when=asyncio.FIRST_COMPLETED)
    if call.done():
        seen.cancel()
        raise AssertionError(f"the call ended before the command printed {marker!r}: {_outcome(call)}")
    call.cancel()
    await asyncio.wait({call})
    # A marker that never came timed seen out, and that is the failure to report.
    await seen
    if not call.cancelled():
        raise AssertionError(f"a cancel ended the call with {_outcome(call)}")


def catch_strays(strays: list[str]) -> None:
    """Keep every error no task awaited, so the check that left it fails instead of a log line nobody reads."""

    def handler(loop: asyncio.AbstractEventLoop, context: dict[str, Any]) -> None:
        err = context.get("exception")
        strays.append(f"{type(err).__name__}: {err}" if err is not None else str(context.get("message")))

    asyncio.get_running_loop().set_exception_handler(handler)


def _outcome(call: asyncio.Future[object]) -> str:
    err = call.exception()
    if err is not None:
        return f"{type(err).__name__}: {err}"
    return repr(call.result())
