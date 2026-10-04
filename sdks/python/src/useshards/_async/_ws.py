"""One WebSocket to the daemon, on a connection the HTTPX client opened and upgraded."""

from __future__ import annotations

import errno
import socket
from collections import deque

import httpcore
import httpx

from .._frames import (
    CLOSE_NORMAL,
    OP_BINARY,
    OP_CLOSE,
    OP_PING,
    OP_PONG,
    Message,
    MessageReader,
    accept_for,
    encode_close,
    encode_frame,
    handshake_key,
    parse_close,
)
from .._wire import connection_error
from ..errors import ProtocolError, ShardConnectionError
from . import _backend

_READ_SIZE = 64 * 1024
# How long a goodbye waits for the daemon's own close before the connection is let go.
CLOSE_WAIT = 5.0
_STREAM_ERRORS = (httpcore.NetworkError, httpcore.TimeoutException, OSError)


class AsyncWebSocket:
    def __init__(self, response: httpx.Response, stream: httpcore.AsyncNetworkStream, what: str) -> None:
        self._response = response
        self._stream = stream
        self._what = what
        self._reader = MessageReader()
        self._inbox: deque[Message] = deque()
        self._send_lock = _backend.Lock()
        self._close_sent = False
        self._peer_closed = False
        self._dropped = False
        self.close_code: int | None = None
        self.close_reason = ""

    @classmethod
    async def connect(
        cls, client: httpx.AsyncClient, path: str, what: str, params: dict[str, str] | None = None
    ) -> AsyncWebSocket:
        key = handshake_key()
        headers = {
            "Connection": "Upgrade",
            "Upgrade": "websocket",
            "Sec-WebSocket-Version": "13",
            "Sec-WebSocket-Key": key,
        }
        request = client.build_request("GET", path, params=params, headers=headers)
        try:
            response = await client.send(request, stream=True)
        except httpx.TransportError as e:
            raise connection_error(request, e) from e
        # The client's response hook already raised every refusal, so what is left here is a 2xx.
        if response.status_code != 101:
            await response.aclose()
            raise ProtocolError(f"{what}: the daemon answered {response.status_code} where a WebSocket upgrade belongs")
        stream = response.extensions.get("network_stream")
        if response.headers.get("sec-websocket-accept") != accept_for(key) or not isinstance(
            stream, httpcore.AsyncNetworkStream
        ):
            await response.aclose()
            raise ProtocolError(f"{what}: the daemon answered the WebSocket handshake with a key that does not match")
        return cls(response, stream, what)

    @property
    def ended(self) -> bool:
        """The daemon's close arrived, or the connection ended: nothing more arrives."""
        return self._peer_closed or self._dropped

    async def send_binary(self, payload: bytes) -> None:
        await self._send(encode_frame(OP_BINARY, payload))

    async def receive(self) -> Message | None:
        """The next text or binary message, or None once the daemon closed the stream."""
        while True:
            if not self._inbox:
                if self.ended:
                    return None
                await self._fill(None)
                continue
            message = self._inbox.popleft()
            if message.opcode == OP_PING:
                await self._send(encode_frame(OP_PONG, message.payload))
                continue
            if message.opcode == OP_PONG:
                continue
            if message.opcode == OP_CLOSE:
                self._closed_by_peer(message.payload)
                return None
            return message

    async def send_close(self) -> None:
        if self._close_sent:
            return
        self._close_sent = True
        await self._send(encode_close(CLOSE_NORMAL))

    async def close(self) -> None:
        """Say goodbye, or answer the daemon's, wait a moment for its close, then let go of the connection."""
        try:
            if not self._dropped:
                await self.send_close()
            await self._drain()
        finally:
            await self.release()

    async def release(self) -> None:
        """Let go of the connection at once. A read blocked in another thread wakes only on a shutdown."""
        sock = self._stream.get_extra_info("socket")
        try:
            if isinstance(sock, socket.socket):
                _shutdown(sock, self._what)
        finally:
            await self._response.aclose()

    async def _drain(self) -> None:
        while not self.ended:
            await self._fill(CLOSE_WAIT)
            self._discard_inbox()

    def _discard_inbox(self) -> None:
        """Drop every message but a close, which ends the drain."""
        while self._inbox:
            message = self._inbox.popleft()
            if message.opcode == OP_CLOSE:
                self._closed_by_peer(message.payload)

    def _closed_by_peer(self, payload: bytes) -> None:
        self._peer_closed = True
        self.close_code, self.close_reason = parse_close(payload)

    async def _fill(self, timeout: float | None) -> None:
        try:
            data = await self._stream.read(_READ_SIZE, timeout=timeout)
        except _STREAM_ERRORS as e:
            self._dropped = True
            raise ShardConnectionError(f"{self._what}: the stream to the daemon dropped") from e
        if not data:
            self._dropped = True
            return
        self._inbox.extend(self._reader.feed(data))

    async def _send(self, frame: bytes) -> None:
        async with self._send_lock:
            try:
                await self._stream.write(frame, timeout=None)
            except _STREAM_ERRORS as e:
                raise ShardConnectionError(f"{self._what}: the stream to the daemon dropped") from e


def _shutdown(sock: socket.socket, what: str) -> None:
    try:
        sock.shutdown(socket.SHUT_RDWR)
    except OSError as e:
        # The connection is already gone, which is all a release asks for.
        if e.errno in (errno.ENOTCONN, errno.EBADF):
            return
        raise ShardConnectionError(f"{what}: shut the stream to the daemon: {e.strerror}") from e
