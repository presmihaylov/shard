"""The output a command result keeps: the newest bytes of stdout and stderr together, up to a limit."""

from __future__ import annotations

from collections import deque

from ._wire import STDERR, STDOUT

DEFAULT_OUTPUT_LIMIT = 8 * 1024 * 1024


class OutputCapture:
    def __init__(self, limit: int) -> None:
        if limit < 0:
            raise ValueError("output_limit_bytes must be 0 or more")
        self._limit = limit
        self._chunks: deque[tuple[int, bytes]] = deque()
        self._size = 0

    def add(self, stream: int, data: bytes) -> None:
        if self._limit == 0 or not data:
            return
        if len(data) >= self._limit:
            self._chunks.clear()
            self._chunks.append((stream, data[len(data) - self._limit :]))
            self._size = self._limit
            return
        self._chunks.append((stream, data))
        self._size += len(data)
        while self._size > self._limit:
            head_stream, head = self._chunks[0]
            over = self._size - self._limit
            if len(head) <= over:
                self._chunks.popleft()
                self._size -= len(head)
                continue
            self._chunks[0] = (head_stream, head[over:])
            self._size -= over

    def output(self) -> tuple[bytes, bytes]:
        out = b"".join(chunk for stream, chunk in self._chunks if stream == STDOUT)
        err = b"".join(chunk for stream, chunk in self._chunks if stream == STDERR)
        return out, err
