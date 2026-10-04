from __future__ import annotations

import asyncio
import io
import json
import os
import stat
import tarfile
from collections.abc import AsyncIterator, Callable, Iterator
from pathlib import Path

import httpx
import pytest

from useshards import _archive
from useshards._async._files import AsyncFiles
from useshards._async._transport import AsyncTransport
from useshards._config import Settings
from useshards._sync._files import Files
from useshards._sync._transport import CHUNK, Transport
from useshards.errors import NotFoundError, ProtocolError, ShardConnectionError, UnknownLengthError

BASE = "http://shard.test"
STAT = {"type": "file", "size": 5, "mode": 0o640, "uid": 0, "gid": 0, "mtime": "2026-10-04T10:00:00.123456789Z"}
DIR_STAT = {**STAT, "type": "dir", "mode": 0o751}

Handler = Callable[[httpx.Request], httpx.Response]


def files(handler: Handler) -> Files:
    transport = Transport(Settings(base_url=BASE, api_key="k", verify=True), 5.0, httpx.MockTransport(handler))
    return Files(transport, "sb")


class Daemon:
    """Records every request the fake daemon took, with the body it read."""

    def __init__(self, answer: Handler | None = None) -> None:
        self.requests: list[httpx.Request] = []
        self.bodies: list[bytes] = []
        self._answer = answer

    def __call__(self, request: httpx.Request) -> httpx.Response:
        self.bodies.append(request.read())
        self.requests.append(request)
        if self._answer is not None:
            return self._answer(request)
        return httpx.Response(204)


class Cut(httpx.SyncByteStream):
    """A body that sends one chunk and then drops, as an aborted chunked response does."""

    def __iter__(self) -> Iterator[bytes]:
        yield b"half"
        raise httpx.RemoteProtocolError("peer closed connection without sending complete message body")


class Chunks(httpx.AsyncByteStream):
    def __init__(self, chunks: list[bytes]) -> None:
        self._chunks = chunks

    async def __aiter__(self) -> AsyncIterator[bytes]:
        for chunk in self._chunks:
            yield chunk


def test_write_sends_a_known_length() -> None:
    daemon = Daemon()
    files(daemon).write("/tmp/a", "hello", mode=0o750, parents=True, user="nobody")
    request = daemon.requests[0]
    assert (request.method, request.url.path) == ("PUT", "/v0/sandboxes/sb/files")
    assert dict(request.url.params) == {"path": "/tmp/a", "mode": "750", "parents": "true", "user": "nobody"}
    assert request.headers["Content-Length"] == "5"
    assert "Transfer-Encoding" not in request.headers
    assert daemon.bodies == [b"hello"]


def test_write_empty_sends_zero_length() -> None:
    daemon = Daemon()
    files(daemon).write("/tmp/a", b"")
    assert daemon.requests[0].headers["Content-Length"] == "0"


def test_write_seekable_stream_from_its_position() -> None:
    daemon = Daemon()
    source = io.BytesIO(b"skip-then-this")
    source.seek(5)
    files(daemon).write("/tmp/a", source)
    assert daemon.requests[0].headers["Content-Length"] == "9"
    assert "Transfer-Encoding" not in daemon.requests[0].headers
    assert daemon.bodies == [b"then-this"]


class Pipe(io.RawIOBase):
    def __init__(self, data: bytes) -> None:
        self._data = io.BytesIO(data)

    def readable(self) -> bool:
        return True

    def readinto(self, buffer: memoryview) -> int:  # type: ignore[override]
        chunk = self._data.read(len(buffer))
        buffer[: len(chunk)] = chunk
        return len(chunk)


def test_write_unknown_length_refused() -> None:
    daemon = Daemon()
    with pytest.raises(UnknownLengthError, match="pass size="):
        files(daemon).write("/tmp/a", io.BufferedReader(Pipe(b"data")))
    assert daemon.requests == []


def test_write_stream_with_size_sends_only_size() -> None:
    daemon = Daemon()
    files(daemon).write("/tmp/a", io.BufferedReader(Pipe(b"data and more")), size=4)
    assert daemon.bodies == [b"data"]


def test_write_stream_short_of_size() -> None:
    with pytest.raises(UnknownLengthError, match="ended 6 bytes short of the 10"):
        files(Daemon()).write("/tmp/a", io.BufferedReader(Pipe(b"data")), size=10)


def test_write_bytes_size_mismatch() -> None:
    with pytest.raises(ValueError, match="size=3"):
        files(Daemon()).write("/tmp/a", b"data", size=3)


def test_upload_takes_the_local_mode(tmp_path: Path) -> None:
    local = tmp_path / "up.bin"
    local.write_bytes(os.urandom(3 * CHUNK + 7))
    local.chmod(0o751)
    daemon = Daemon()
    files(daemon).upload(local, "/tmp/up.bin")
    request = daemon.requests[0]
    assert request.url.params["mode"] == "751"
    assert request.headers["Content-Length"] == str(local.stat().st_size)
    assert daemon.bodies == [local.read_bytes()]


def test_upload_refuses_a_fifo(tmp_path: Path) -> None:
    fifo = tmp_path / "fifo"
    os.mkfifo(fifo)
    writer = os.open(fifo, os.O_RDWR)
    try:
        with pytest.raises(UnknownLengthError, match="not a regular file"):
            files(Daemon()).upload(fifo, "/tmp/fifo")
    finally:
        os.close(writer)


def test_download(tmp_path: Path) -> None:
    def answer(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, headers={"X-Shard-Stat": json.dumps(STAT)}, content=b"bytes")

    target = tmp_path / "down.bin"
    files(Daemon(answer)).download("/tmp/f", target)
    assert target.read_bytes() == b"bytes"
    assert stat.S_IMODE(target.stat().st_mode) == 0o640
    assert sorted(p.name for p in tmp_path.iterdir()) == ["down.bin"]


def test_download_cut_leaves_nothing(tmp_path: Path) -> None:
    def answer(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, headers={"X-Shard-Stat": json.dumps(STAT)}, stream=Cut())

    target = tmp_path / "down.bin"
    target.write_bytes(b"old")
    with pytest.raises(ShardConnectionError, match="/v0/sandboxes/sb/files"):
        files(Daemon(answer)).download("/tmp/f", target)
    assert target.read_bytes() == b"old"
    assert sorted(p.name for p in tmp_path.iterdir()) == ["down.bin"]


def test_download_refused(tmp_path: Path) -> None:
    def answer(request: httpx.Request) -> httpx.Response:
        return httpx.Response(404, json={"error": {"code": "not_found", "message": "no such file"}})

    with pytest.raises(NotFoundError, match="no such file"):
        files(Daemon(answer)).download("/tmp/f", tmp_path / "down.bin")
    assert list(tmp_path.iterdir()) == []


def test_stat_names_the_path_a_head_cannot() -> None:
    def answer(request: httpx.Request) -> httpx.Response:
        return httpx.Response(404)

    with pytest.raises(NotFoundError, match="404 to a stat of /tmp/missing"):
        files(Daemon(answer)).stat("/tmp/missing")


def test_stat() -> None:
    def answer(request: httpx.Request) -> httpx.Response:
        assert request.method == "HEAD"
        return httpx.Response(200, headers={"X-Shard-Stat": json.dumps(STAT)})

    info = files(Daemon(answer)).stat("/tmp/f")
    assert (info.type, info.size, info.mode) == ("file", 5, 0o640)
    assert info.mtime.tzinfo is not None


def test_list_cut_short() -> None:
    def answer(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, content=b'{"entries":[{"name":"a",' + json.dumps(STAT)[1:].encode())

    with pytest.raises(ProtocolError, match="cut the listing of /tmp short"):
        files(Daemon(answer)).list("/tmp")


def test_list() -> None:
    def answer(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, json={"entries": [{"name": "a", **STAT}, {"name": "b", **DIR_STAT}]})

    assert [(e.name, e.type) for e in files(Daemon(answer)).list("/tmp")] == [("a", "file"), ("b", "dir")]


def test_mkdir_and_remove() -> None:
    daemon = Daemon()
    sandbox = files(daemon)
    sandbox.mkdir("/tmp/a/b", mode=0o700, parents=True)
    sandbox.remove("/tmp/a", recursive=True)
    assert json.loads(daemon.bodies[0]) == {"path": "/tmp/a/b", "mode": "700", "parents": True}
    assert dict(daemon.requests[1].url.params) == {"path": "/tmp/a", "recursive": "true"}


def test_upload_dir(tmp_path: Path) -> None:
    source = tmp_path / "source"
    (source / "nested").mkdir(parents=True)
    (source / "nested" / "leaf.txt").write_bytes(b"leaf\n")
    daemon = Daemon()
    files(daemon).upload_dir(source, "/srv/app/")
    request = daemon.requests[0]
    assert (request.method, request.url.path) == ("PUT", "/v0/sandboxes/sb/archive")
    assert dict(request.url.params) == {"path": "/srv"}
    assert request.headers["Content-Length"] == str(len(daemon.bodies[0]))
    with tarfile.open(fileobj=io.BytesIO(daemon.bodies[0])) as t:
        assert sorted(t.getnames()) == ["app", "app/nested", "app/nested/leaf.txt"]


@pytest.mark.parametrize("remote", ["/", "relative/dir", "/.."])
def test_dir_transfer_needs_a_named_directory(tmp_path: Path, remote: str) -> None:
    with pytest.raises(ValueError, match="absolute sandbox path below /"):
        files(Daemon()).upload_dir(tmp_path, remote)


def packed(tree: Path, name: str) -> bytes:
    out = io.BytesIO()
    _archive.pack(str(tree), name, out)
    return out.getvalue()


def test_download_dir(tmp_path: Path) -> None:
    tree = tmp_path / "tree"
    (tree / "deeper").mkdir(parents=True)
    (tree / "deeper" / "leaf.txt").write_bytes(b"leaf\n")

    def answer(request: httpx.Request) -> httpx.Response:
        assert request.url.params["path"] == "/srv/app"
        return httpx.Response(200, headers={"X-Shard-Stat": json.dumps(DIR_STAT)}, content=packed(tree, "app"))

    back = tmp_path / "back"
    files(Daemon(answer)).download_dir("/srv/app/", back)
    assert (back / "deeper" / "leaf.txt").read_bytes() == b"leaf\n"
    assert stat.S_IMODE(back.stat().st_mode) == 0o751


def test_download_dir_cut_lands_nothing(tmp_path: Path) -> None:
    def answer(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, headers={"X-Shard-Stat": json.dumps(DIR_STAT)}, stream=Cut())

    with pytest.raises(ShardConnectionError):
        files(Daemon(answer)).download_dir("/srv/app", tmp_path / "back")
    assert list(tmp_path.iterdir()) == []


def test_download_dir_of_a_file(tmp_path: Path) -> None:
    def answer(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, headers={"X-Shard-Stat": json.dumps(STAT)}, content=b"")

    with pytest.raises(NotADirectoryError, match="it is a file"):
        files(Daemon(answer)).download_dir("/srv/app", tmp_path / "back")


def test_async_write_and_download(tmp_path: Path) -> None:
    seen: list[bytes] = []

    async def handler(request: httpx.Request) -> httpx.Response:
        if request.method == "PUT":
            seen.append(await request.aread())
            assert "Transfer-Encoding" not in request.headers
            return httpx.Response(204)
        return httpx.Response(200, headers={"X-Shard-Stat": json.dumps(STAT)}, stream=Chunks([b"by", b"tes"]))

    async def run() -> None:
        transport = AsyncTransport(Settings(base_url=BASE, api_key="k", verify=True), 5.0, httpx.MockTransport(handler))
        sandbox = AsyncFiles(transport, "sb")
        await sandbox.write("/tmp/a", io.BytesIO(b"async body"))
        await sandbox.download("/tmp/a", tmp_path / "down.bin")
        await transport.aclose()

    asyncio.run(run())
    assert seen == [b"async body"]
    assert (tmp_path / "down.bin").read_bytes() == b"bytes"
