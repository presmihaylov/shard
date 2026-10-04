"""A log as it arrives: one WebSocket, opened on the first read and let go of by the end, a fault or a close."""

from __future__ import annotations

import json
from collections.abc import Callable
from typing import Generic, Self, TypeVar

from .._frames import CLOSE_NORMAL, OP_BINARY, OP_TEXT, Message
from .._generated import models
from .._types import EgressDecision, egress_decision
from .._wire import EXIT, FAILURE, STDOUT, failure_of
from ..errors import ProtocolError, ShardConnectionError, failure_error
from . import _backend
from ._transport import AsyncTransport
from ._ws import AsyncWebSocket

T = TypeVar("T")

# What one message carries, or None for the daemon's end of the log.
Parse = Callable[[Message, str], T | None]


class AsyncFollow(Generic[T]):
    """Iterate it, or use it as a context manager that closes it. A close from anywhere ends a read in flight."""

    def __init__(self, transport: AsyncTransport, path: str, what: str, parse: Parse[T]) -> None:
        self._transport = transport
        self._path = path
        self._what = what
        self._parse = parse
        self._lock = _backend.Lock()
        self._ws: AsyncWebSocket | None = None
        self._reading = False
        self._closed = False
        self._over = False

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(self, *_: object) -> None:
        await self.aclose()

    def __aiter__(self) -> Self:
        return self

    async def __anext__(self) -> T:
        async with self._lock:
            if self._closed or self._over:
                raise StopAsyncIteration
            self._reading = True
        try:
            item = await self._next()
        except BaseException as e:
            if await self._end(e):
                raise StopAsyncIteration from None
            raise
        if item is None:
            await self._end(None)
            raise StopAsyncIteration
        async with self._lock:
            self._reading = False
        return item

    async def aclose(self) -> None:
        async with self._lock:
            self._closed = True
            reading = self._reading
            ws, self._ws = self._ws, None
        if ws is None:
            return
        # A read blocked on the socket wakes only when the socket goes, so a polite close would wait on it.
        if reading:
            await ws.release()
            return
        await ws.close()

    async def _next(self) -> T | None:
        ws = self._ws or await self._open()
        message = await ws.receive()
        if message is not None:
            return self._parse(message, self._what)
        if ws.close_code is None:
            raise ShardConnectionError(f"{self._what}: the stream to the daemon dropped")
        if ws.close_code != CLOSE_NORMAL:
            raise failure_error("internal", f"{self._what}: {ws.close_reason or f'closed with {ws.close_code}'}")
        return None

    async def _open(self) -> AsyncWebSocket:
        ws = await AsyncWebSocket.connect(self._transport.http, self._path, self._what, {"follow": "true"})
        async with self._lock:
            if not self._closed:
                self._ws = ws
                return ws
        await ws.release()
        raise StopAsyncIteration

    async def _end(self, fault: BaseException | None) -> bool:
        """Let go of the stream once the read is over, and answer whether a close came first."""
        async with self._lock:
            self._reading = False
            self._over = True
            closed = self._closed
            ws, self._ws = self._ws, None
        if ws is None:
            return closed
        if fault is None:
            await ws.close()
            return closed
        try:
            await ws.release()
        except Exception as release_error:
            fault.add_note(f"letting go of the stream also failed: {release_error}")
        return closed


def log_chunk(message: Message, what: str) -> bytes | None:
    payload = message.payload
    if message.opcode == OP_BINARY and payload[:1] == bytes([STDOUT]):
        return payload[1:]
    if message.opcode == OP_BINARY and payload[:1] == bytes([EXIT]):
        return None
    if message.opcode == OP_BINARY and payload[:1] == bytes([FAILURE]):
        raise failure_of(payload[1:], what)
    raise ProtocolError(f"{what}: the daemon sent a message the SDK cannot read: {payload[:64]!r}")


def egress_log_entry(message: Message, what: str) -> EgressDecision:
    if message.opcode != OP_TEXT:
        raise ProtocolError(f"{what}: the daemon sent a binary message where a decision belongs")
    try:
        decision = models.EgressDecision.from_dict(json.loads(message.payload))
    except (KeyError, TypeError, ValueError):
        raise ProtocolError(f"{what}: the daemon sent a decision the SDK cannot read") from None
    return egress_decision(decision)
