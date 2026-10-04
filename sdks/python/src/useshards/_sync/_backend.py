"""What the sync client runs on: threads. The async client's twin of this file runs on asyncio."""

from __future__ import annotations

import threading
import time
from collections.abc import Callable
from typing import TypeVar

T = TypeVar("T")

Lock = threading.Lock
Event = threading.Event
sleep = time.sleep


def offload(fn: Callable[[], T]) -> T:
    return fn()


def wait_event(event: threading.Event, timeout: float | None) -> bool:
    return event.wait(timeout)


class Task:
    """One background job, as a daemon thread, so a command left running never holds the interpreter open."""

    def __init__(self, fn: Callable[[], None]) -> None:
        self._thread = threading.Thread(target=fn, daemon=True)
        self._thread.start()

    def join(self, timeout: float | None) -> bool:
        self._thread.join(timeout)
        return not self._thread.is_alive()

    def cancel(self) -> None:
        # A thread cannot be cancelled; the caller ends its blocking read by shutting the socket.
        return
