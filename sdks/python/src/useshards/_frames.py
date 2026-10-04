"""RFC 6455 framing for the daemon's streams: the client masks every frame, the daemon masks none."""

from __future__ import annotations

import base64
import hashlib
import os
import struct

import attrs

from .errors import ProtocolError

OP_CONTINUATION = 0x0
OP_TEXT = 0x1
OP_BINARY = 0x2
OP_CLOSE = 0x8
OP_PING = 0x9
OP_PONG = 0xA

CLOSE_NORMAL = 1000

_ACCEPT_GUID = b"258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
_CONTROL_LIMIT = 125
# The daemon sends a stream id and at most 1 MiB, so four times that is a broken peer, not a message.
MESSAGE_LIMIT = 4 * 1024 * 1024


def handshake_key() -> str:
    return base64.b64encode(os.urandom(16)).decode("ascii")


def accept_for(key: str) -> str:
    return base64.b64encode(hashlib.sha1(key.encode("ascii") + _ACCEPT_GUID).digest()).decode("ascii")


def encode_frame(opcode: int, payload: bytes) -> bytes:
    length = len(payload)
    if length < 126:
        header = struct.pack("!BB", 0x80 | opcode, 0x80 | length)
    elif length < 1 << 16:
        header = struct.pack("!BBH", 0x80 | opcode, 0x80 | 126, length)
    else:
        header = struct.pack("!BBQ", 0x80 | opcode, 0x80 | 127, length)
    mask = os.urandom(4)
    return header + mask + _masked(payload, mask)


def encode_close(code: int, reason: str = "") -> bytes:
    return encode_frame(OP_CLOSE, struct.pack("!H", code) + reason.encode("utf-8")[: _CONTROL_LIMIT - 2])


def parse_close(payload: bytes) -> tuple[int | None, str]:
    if len(payload) < 2:
        return None, ""
    (code,) = struct.unpack("!H", payload[:2])
    return code, payload[2:].decode("utf-8", "replace")


def _masked(payload: bytes, mask: bytes) -> bytes:
    if not payload:
        return b""
    # One XOR over the whole payload as a big integer is far faster in pure Python than a byte loop.
    key = (mask * (len(payload) // 4 + 1))[: len(payload)]
    xored = int.from_bytes(payload, "big") ^ int.from_bytes(key, "big")
    return xored.to_bytes(len(payload), "big")


@attrs.frozen
class Message:
    opcode: int
    payload: bytes


class MessageReader:
    """Turns the daemon's bytes into whole messages, with control frames passed through as they arrive."""

    def __init__(self) -> None:
        self._buffer = bytearray()
        self._opcode: int | None = None
        self._fragments: list[bytes] = []
        self._fragmented = 0

    def feed(self, data: bytes) -> list[Message]:
        self._buffer += data
        messages: list[Message] = []
        while True:
            frame = self._next_frame()
            if frame is None:
                return messages
            fin, opcode, payload = frame
            message = self._assemble(fin, opcode, payload)
            if message is not None:
                messages.append(message)

    def _next_frame(self) -> tuple[bool, int, bytes] | None:
        buf = self._buffer
        if len(buf) < 2:
            return None
        first, second = buf[0], buf[1]
        if first & 0x70:
            raise ProtocolError("the daemon sent a WebSocket frame with a reserved bit set")
        if second & 0x80:
            raise ProtocolError("the daemon sent a masked WebSocket frame, which only a client sends")
        fin = bool(first & 0x80)
        opcode = first & 0x0F
        short = second & 0x7F
        length, offset = short, 2
        if short == 126:
            if len(buf) < 4:
                return None
            (length,) = struct.unpack_from("!H", buf, 2)
            offset = 4
        if short == 127:
            if len(buf) < 10:
                return None
            (length,) = struct.unpack_from("!Q", buf, 2)
            offset = 10
        if opcode >= OP_CLOSE and (length > _CONTROL_LIMIT or not fin):
            raise ProtocolError("the daemon sent a WebSocket control frame that is fragmented or too long")
        if length > MESSAGE_LIMIT:
            raise ProtocolError(f"the daemon sent a WebSocket frame of {length} bytes, over {MESSAGE_LIMIT}")
        if len(buf) < offset + length:
            return None
        payload = bytes(buf[offset : offset + length])
        del buf[: offset + length]
        return fin, opcode, payload

    def _assemble(self, fin: bool, opcode: int, payload: bytes) -> Message | None:
        if opcode not in (OP_CONTINUATION, OP_TEXT, OP_BINARY, OP_CLOSE, OP_PING, OP_PONG):
            raise ProtocolError(f"the daemon sent a WebSocket frame of unknown opcode {opcode}")
        if opcode >= OP_CLOSE:
            return Message(opcode, payload)
        kind = self._opcode
        if opcode == OP_CONTINUATION and kind is None:
            raise ProtocolError("the daemon sent a continuation frame with no message to continue")
        if opcode != OP_CONTINUATION and kind is not None:
            raise ProtocolError("the daemon started a WebSocket message inside another")
        if kind is None:
            kind = opcode
        self._fragmented += len(payload)
        if self._fragmented > MESSAGE_LIMIT:
            raise ProtocolError(f"the daemon sent a WebSocket message over {MESSAGE_LIMIT} bytes")
        self._fragments.append(payload)
        if not fin:
            self._opcode = kind
            return None
        message = Message(kind, b"".join(self._fragments))
        self._opcode = None
        self._fragments = []
        self._fragmented = 0
        return message
