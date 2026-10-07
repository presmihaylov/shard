"""What only the sync mode does: the clock, the background read, and how a check cuts a call short."""

from __future__ import annotations

import socket
import threading
import time
from collections.abc import Callable, Iterator
from typing import Protocol, TypeVar

from useshards import OutputCallback

T = TypeVar("T", covariant=True)
C = TypeVar("C")

Call = Callable[[], object]
Probe = Callable[[], bool]
Body = Callable[[C], None]

SEEN_WITHIN = 60.0


class Cancelled(Exception):
    """What a cancelled background read answers once the close ended it cleanly."""


class Source(Protocol[T]):
    def __iter__(self) -> Iterator[T]: ...

    def close(self) -> None: ...


def sleep(seconds: float) -> None:
    time.sleep(seconds)


def within(seconds: float, run: Call) -> None:
    # A thread cannot be cancelled, so a check past its time is left behind as a daemon thread.
    outcome: list[BaseException] = []

    def body() -> None:
        try:
            run()
        except BaseException as e:
            outcome.append(e)

    thread = threading.Thread(target=body, daemon=True)
    thread.start()
    thread.join(seconds)
    if thread.is_alive():
        raise AssertionError(f"the check did not end within {seconds:g} s")
    if outcome:
        raise outcome[0]


class Pending:
    """A read left running in the background, which answers what ended it so an error is never lost unseen."""

    def __init__(self, source: Source[object], read: Callable[[], None]) -> None:
        self._source = source
        self._outcome: list[BaseException] = []
        self._cancelled = False
        self._thread = threading.Thread(target=self._run, args=(read,), daemon=True)
        self._thread.start()

    def _run(self, read: Callable[[], None]) -> None:
        try:
            read()
        except Exception as e:
            self._outcome.append(e)

    def result(self) -> BaseException | None:
        self._thread.join()
        if self._outcome:
            return self._outcome[0]
        if self._cancelled:
            return Cancelled()
        return None

    def cancel(self) -> None:
        # A thread blocked on a read only wakes when the socket under it closes.
        self._cancelled = True
        self._source.close()


def consume(source: Source[T], each: Callable[[T], None]) -> Pending:
    def read() -> None:
        try:
            for item in source:
                each(item)
        finally:
            source.close()

    return Pending(source, read)


class Printed:
    """An output callback that marks the point a command printed the given text."""

    def __init__(self, want: str) -> None:
        self._want = want.encode()
        self._out = b""
        self._seen = threading.Event()

    def on_stdout(self, chunk: bytes) -> None:
        self._out += chunk
        if self._want in self._out:
            self._seen.set()

    @property
    def done(self) -> bool:
        return self._seen.is_set()

    def seen(self) -> None:
        if not self._seen.wait(SEEN_WITHIN):
            raise AssertionError(f"the command did not print {self._want!r} within {SEEN_WITHIN:g} s")


def greeting(host_port: int) -> bytes:
    """The first line the guest sends through a host port of this host, or what it sent before it closed."""
    with socket.create_connection(("127.0.0.1", host_port), timeout=5) as conn, conn.makefile("rb") as stream:
        return stream.readline()


def catch_strays(strays: list[str]) -> None:
    """Keep every error a background thread raised, so the check that left it fails instead of a log line."""

    def hook(args: threading.ExceptHookArgs) -> None:
        strays.append(f"{args.exc_type.__name__}: {args.exc_value}")

    threading.excepthook = hook


class _Cancel(Exception):
    """Raised from an output callback, which is how a sync caller cuts a call short."""


def cancel_after(run: Callable[[OutputCallback], object], marker: str) -> None:
    """Raise from the callback once the command printed the marker, and fail unless that is what ended the call."""
    printed = Printed(marker)

    def on_stdout(chunk: bytes) -> None:
        printed.on_stdout(chunk)
        if printed.done:
            raise _Cancel

    try:
        result = run(on_stdout)
    except _Cancel:
        return
    raise AssertionError(f"the call ended before the command printed {marker!r}: {result!r}")
