"""A daemon that speaks just enough HTTP and WebSocket for the client, on a real socket."""

from __future__ import annotations

import json
import socket
import struct
import threading
from collections.abc import Callable
from typing import Any

from useshards._frames import OP_BINARY, OP_CLOSE, OP_PING, OP_PONG, OP_TEXT, accept_for
from useshards._wire import EXIT, FAILURE, STDIN, STDIN_CLOSE

RECORD: dict[str, Any] = {
    "exec": "e1",
    "sandbox": "sb",
    "command": ["/bin/sh", "-c", "true"],
    "state": "running",
    "exit_status": None,
    "started_at": "2026-10-04T10:00:00Z",
    "exited_at": None,
    "lost_bytes": 0,
    "truncated": False,
}
IN_USE = (409, {"error": {"code": "in_use", "message": "another client is attached to e1"}})
NOT_STARTED = (
    422,
    {"error": {"code": "command_not_started", "message": 'sandbox sb could not run "/no/such"', "exit_code": 127}},
)


class Peer:
    """The daemon's end of one command stream."""

    def __init__(self, conn: Conn) -> None:
        self._conn = conn

    def send(self, stream: int, payload: bytes = b"") -> None:
        self.frame(OP_BINARY, bytes([stream]) + payload)

    def text(self, record: dict[str, Any]) -> None:
        self.frame(OP_TEXT, json.dumps(record).encode())

    def close(self, code: int, reason: str = "") -> str:
        """Close first, as the daemon does at the end of a follow, and read the client's answer."""
        self.frame(OP_CLOSE, struct.pack("!H", code) + reason.encode())
        return self.until_end()

    def frame(self, opcode: int, payload: bytes) -> None:
        n = len(payload)
        head = struct.pack("!BB", 0x80 | opcode, n) if n < 126 else struct.pack("!BBH", 0x80 | opcode, 126, n)
        self._conn.sock.sendall(head + payload)

    def recv(self) -> tuple[int, bytes] | None:
        """The client's next frame unmasked, or None at EOF."""
        head = self._conn.read(2)
        if head is None:
            return None
        n = head[1] & 0x7F
        if n == 126:
            n = struct.unpack("!H", self._conn.need(2))[0]
        if n == 127:
            n = struct.unpack("!Q", self._conn.need(8))[0]
        mask = self._conn.need(4)
        data = self._conn.need(n)
        return head[0] & 0x0F, bytes(b ^ mask[i % 4] for i, b in enumerate(data))

    def stdin(self) -> list[bytes]:
        """Every input piece up to the client's stdin close."""
        pieces: list[bytes] = []
        while True:
            frame = self.recv()
            assert frame is not None, "the client left before it closed stdin"
            _, payload = frame
            if payload[0] == STDIN_CLOSE:
                return pieces
            assert payload[0] == STDIN
            pieces.append(payload[1:])

    def finish(self, code: int = 0, **extra: Any) -> str:
        self.send(EXIT, json.dumps({"code": code, **extra}).encode())
        return self.until_end()

    def finish_without_close(self, code: int = 0) -> None:
        """Send the exit and read nothing more, as a daemon whose command ended without reading its input."""
        self.send(EXIT, json.dumps({"code": code}).encode())

    def fail(self, code: str, message: str) -> str:
        self.send(FAILURE, json.dumps({"error": {"code": code, "message": message}}).encode())
        return self.until_end()

    def until_end(self) -> str:
        """Read until the client says goodbye, answered as the daemon does, or drops: "close" or "eof"."""
        while True:
            frame = self.recv()
            if frame is None:
                return "eof"
            opcode, payload = frame
            if opcode == OP_CLOSE:
                self.frame(OP_CLOSE, payload[:2])
                return "close"


Session = Callable[[Peer], Any]
Answer = tuple[int, Any]


class Conn:
    def __init__(self, sock: socket.socket) -> None:
        self.sock = sock
        self._buffer = b""

    def read(self, n: int) -> bytes | None:
        while len(self._buffer) < n:
            data = self.sock.recv(65536)
            if not data:
                return None
            self._buffer += data
        out, self._buffer = self._buffer[:n], self._buffer[n:]
        return out

    def need(self, n: int) -> bytes:
        data = self.read(n)
        assert data is not None, "the client left mid-frame"
        return data

    def head(self) -> bytes | None:
        while b"\r\n\r\n" not in self._buffer:
            data = self.sock.recv(65536)
            if not data:
                return None
            self._buffer += data
        head, self._buffer = self._buffer.split(b"\r\n\r\n", 1)
        return head


class FakeDaemon:
    """Answers one request per connection; each WebSocket attach takes the next entry of attaches."""

    def __init__(self) -> None:
        self._server = socket.create_server(("127.0.0.1", 0))
        self.url = f"http://127.0.0.1:{self._server.getsockname()[1]}"
        self.attaches: list[Session | tuple[int, dict[str, Any]]] = []
        self.refuse_when_out: tuple[int, dict[str, Any]] | None = None
        self.bad_accept = False
        self.create: tuple[int, dict[str, Any]] = (201, RECORD)
        self.record: tuple[int, dict[str, Any]] = (200, RECORD)
        self.requests: list[tuple[str, str, bytes]] = []
        self.targets: list[str] = []
        self.routes: dict[tuple[str, str], Answer | Callable[[], Answer | None]] = {}
        self.outcomes: list[Any] = []
        self.errors: list[BaseException] = []
        self._threads: list[threading.Thread] = []
        self._lock = threading.Lock()
        threading.Thread(target=self._accept, daemon=True).start()

    def close(self, timeout: float = 5.0) -> None:
        self._server.close()
        for thread in self._threads:
            thread.join(timeout)

    def _accept(self) -> None:
        while True:
            try:
                sock, _ = self._server.accept()
            except OSError:
                return
            sock.settimeout(5.0)
            thread = threading.Thread(target=self._handle, args=(sock,), daemon=True)
            self._threads.append(thread)
            thread.start()

    def _handle(self, sock: socket.socket) -> None:
        try:
            with sock:
                self._request(Conn(sock))
        except BaseException as e:
            self.errors.append(e)

    def _request(self, conn: Conn) -> None:
        head = conn.head()
        if head is None:
            return
        line, *fields = head.decode().split("\r\n")
        method, target, _ = line.split(" ")
        headers = {k.lower(): v.strip() for k, v in (f.split(":", 1) for f in fields)}
        body = conn.need(int(headers.get("content-length", "0"))) if headers.get("content-length") else b""
        path = target.split("?", 1)[0]
        self.requests.append((method, path, body))
        self.targets.append(target)
        if headers.get("upgrade") == "websocket":
            return self._attach(conn, headers["sec-websocket-key"])
        route = self.routes.get((method, target)) or self.routes.get((method, path))
        if route is not None:
            answer = route() if callable(route) else route
            if answer is not None:
                _answer(conn, *answer)
            return None
        if method == "POST" and path == "/v0/sandboxes/sb/exec":
            return _answer(conn, *self.create)
        if method == "GET" and path == "/v0/sandboxes/sb/exec/e1":
            return _answer(conn, *self.record)
        if method == "POST" and path == "/v0/sandboxes/sb/exec/e1/kill":
            return _answer(conn, 204, None)
        _answer(conn, 404, {"error": {"code": "not_found", "message": f"no route {method} {path}"}})

    def _attach(self, conn: Conn, key: str) -> None:
        with self._lock:
            entry = self.attaches.pop(0) if self.attaches else self.refuse_when_out
        if entry is None:
            return _answer(conn, 500, {"error": {"code": "internal", "message": "the test set no attach"}})
        if isinstance(entry, tuple):
            return _answer(conn, *entry)
        accept = "bm90IHRoZSBrZXk=" if self.bad_accept else accept_for(key)
        conn.sock.sendall(
            "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
            f"Sec-WebSocket-Accept: {accept}\r\n\r\n".encode()
        )
        self.outcomes.append(entry(Peer(conn)))


def _answer(conn: Conn, status: int, body: Any) -> None:
    data, kind = b"", ""
    if isinstance(body, bytes):
        data, kind = body, "Content-Type: text/plain\r\n"
    if body is not None and not isinstance(body, bytes):
        data, kind = json.dumps(body).encode(), "Content-Type: application/json\r\n"
    conn.sock.sendall(
        f"HTTP/1.1 {status} X\r\n{kind}Content-Length: {len(data)}\r\nConnection: close\r\n\r\n".encode() + data
    )


__all__ = ["IN_USE", "NOT_STARTED", "OP_PING", "OP_PONG", "RECORD", "FakeDaemon", "Peer"]
