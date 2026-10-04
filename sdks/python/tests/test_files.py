from __future__ import annotations

import asyncio
import io
import json
import os
import stat
import tarfile
import threading
from collections.abc import AsyncIterator, Awaitable, Callable, Iterator
from pathlib import Path
from typing import IO

import httpx
import pytest

from useshards import _archive
from useshards._async._files import AsyncFiles
from useshards._async._transport import AsyncTransport
from useshards._config import Settings
from useshards._sync._files import Files
from useshards._sync._transport import CHUNK, Transport
from useshards.errors import ConflictError, NotFoundError, ProtocolError, ShardConnectionError, UnknownLengthError

BASE = "http://shard.test"
STAT = {"type": "file", "size": 5, "mode": 0o640, "uid": 0, "gid": 0, "mtime": "2026-10-04T10:00:00.123456789Z"}
DIR_STAT = {**STAT, "type": "dir", "mode": 0o751}

Handler = Callable[[httpx.Request], httpx.Response]


def files(handler: Handler) -> Files:
    return files_over(httpx.MockTransport(handler))


def files_over(http: httpx.BaseTransport) -> Files:
    return Files(Transport(Settings(base_url=BASE, api_key="k", verify=True), 5.0, http), "sb")


class Unread(httpx.BaseTransport, httpx.AsyncBaseTransport):
    """Hands the handler the request with its body unread, as a socket does; MockTransport reads it all first."""

    def __init__(self, handler: Callable[[httpx.Request], Awaitable[httpx.Response] | httpx.Response]) -> None:
        self._handler = handler

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        response = self._handler(request)
        assert isinstance(response, httpx.Response)
        return response

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        response = self._handler(request)
        assert not isinstance(response, httpx.Response)
        return await response


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
    assert [r.extensions["timeout"]["read"] for r in daemon.requests] == [5.0, None]


def test_upload_dir(tmp_path: Path) -> None:
    source = tmp_path / "source"
    (source / "nested").mkdir(parents=True)
    (source / "nested" / "leaf.txt").write_bytes(b"leaf\n")
    daemon = Daemon()
    files(daemon).upload_dir(source, "/srv/app/")
    request = daemon.requests[0]
    assert (request.method, request.url.path) == ("PUT", "/v0/sandboxes/sb/archive")
    assert dict(request.url.params) == {"path": "/srv"}
    assert (request.headers["Transfer-Encoding"], "Content-Length" in request.headers) == ("chunked", False)
    assert request.extensions["timeout"]["read"] is None
    with tarfile.open(fileobj=io.BytesIO(daemon.bodies[0])) as t:
        assert sorted(t.getnames()) == ["app", "app/nested", "app/nested/leaf.txt"]


def test_upload_dir_streams_before_the_pack_ends(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    first = threading.Event()
    waited: list[bool] = []

    def pack(source: str, name: str, out: IO[bytes]) -> None:
        out.write(b"x" * CHUNK)
        out.flush()
        waited.append(first.wait(5))
        out.write(b"y")

    def answer(request: httpx.Request) -> httpx.Response:
        assert isinstance(request.stream, httpx.SyncByteStream)
        got = b""
        for chunk in request.stream:
            got += chunk
            first.set()
        assert got == b"x" * CHUNK + b"y"
        return httpx.Response(204)

    monkeypatch.setattr(_archive, "pack", pack)
    files_over(Unread(answer)).upload_dir(tmp_path, "/srv/app")
    assert waited == [True]


def test_upload_dir_pack_failure_cuts_the_body(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    def pack(source: str, name: str, out: IO[bytes]) -> None:
        out.write(b"x" * CHUNK)
        raise PermissionError(f"{source}/locked")

    def answer(request: httpx.Request) -> httpx.Response:
        request.read()
        return httpx.Response(204)

    monkeypatch.setattr(_archive, "pack", pack)
    with pytest.raises(PermissionError, match="locked"):
        files(answer).upload_dir(tmp_path, "/srv/app")


def refusal_after_one_chunk(packs: list[BaseException]) -> Callable[[str, str, IO[bytes]], None]:
    """A pack bigger than the pipe holds, and what stopped it."""

    def pack(source: str, name: str, out: IO[bytes]) -> None:
        try:
            for _ in range(8):
                out.write(b"x" * CHUNK)
        except BaseException as e:
            packs.append(e)
            raise

    return pack


REFUSAL = {"error": {"code": "sandbox_not_running", "message": "sandbox sb is stopped"}}


def test_upload_dir_refused_mid_tar_stops_the_pack(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    packs: list[BaseException] = []

    def answer(request: httpx.Request) -> httpx.Response:
        assert isinstance(request.stream, httpx.SyncByteStream)
        next(iter(request.stream))
        return httpx.Response(409, json=REFUSAL)

    monkeypatch.setattr(_archive, "pack", refusal_after_one_chunk(packs))
    with pytest.raises(ConflictError, match="is stopped"):
        files_over(Unread(answer)).upload_dir(tmp_path, "/srv/app")
    assert [type(e) for e in packs] == [BrokenPipeError]


def test_async_upload_dir(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    (tmp_path / "tree").mkdir()
    (tmp_path / "tree" / "leaf.txt").write_bytes(b"leaf\n")
    bodies: list[bytes] = []
    packs: list[BaseException] = []

    async def handler(request: httpx.Request) -> httpx.Response:
        assert isinstance(request.stream, httpx.AsyncByteStream)
        assert request.extensions["timeout"]["read"] is None
        if not bodies:
            bodies.append(await request.aread())
            return httpx.Response(204)
        async for _ in request.stream:
            return httpx.Response(409, json=REFUSAL)
        raise AssertionError("the tar sent no bytes")

    async def run() -> None:
        transport = AsyncTransport(Settings(base_url=BASE, api_key="k", verify=True), 5.0, Unread(handler))
        sandbox = AsyncFiles(transport, "sb")
        await sandbox.upload_dir(tmp_path / "tree", "/srv/app")
        monkeypatch.setattr(_archive, "pack", refusal_after_one_chunk(packs))
        with pytest.raises(ConflictError, match="is stopped"):
            await sandbox.upload_dir(tmp_path / "tree", "/srv/app")
        await transport.aclose()

    asyncio.run(run())
    with tarfile.open(fileobj=io.BytesIO(bodies[0])) as t:
        assert sorted(t.getnames()) == ["app", "app/leaf.txt"]
    assert [type(e) for e in packs] == [BrokenPipeError]


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
