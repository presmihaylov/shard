"""What both modes share and what never awaits: assertions, the raw request, and the capture writer."""

from __future__ import annotations

import contextlib
import gc
import os
import re
import shutil
import ssl
import tempfile
import traceback
from collections.abc import Iterator
from pathlib import Path
from typing import Literal

import attrs
import httpx

MiB = 1024 * 1024
Stream = Literal["stdout", "stderr"]


def image() -> str:
    return os.environ.get("SHARD_SUITE_IMAGE") or "alpine:3.20"


class Skip(Exception):
    """A check that cannot run here, such as a verb the provider refuses by name."""


def skip(reason: str) -> Skip:
    return Skip(reason)


def describe(err: BaseException) -> str:
    return f"{type(err).__name__}: {err}"


def _shown(value: object) -> str:
    text = repr(value)
    return text if len(text) <= 200 else f"{text[:200]}... ({len(text)} chars)"


def equal(got: object, want: object, what: str = "") -> None:
    if got != want:
        raise AssertionError(f"{what + ': ' if what else ''}got {_shown(got)}, want {_shown(want)}")


def not_equal(got: object, unwanted: object, what: str = "") -> None:
    if got == unwanted:
        raise AssertionError(f"{what + ': ' if what else ''}got {_shown(got)}, want anything else")


def ok(condition: bool, what: str) -> None:
    if not condition:
        raise AssertionError(what)


def named(name: str | None, what: str) -> str:
    """The name the suite gave, which the record must hold."""
    if name is None:
        raise AssertionError(f"{what} came back with no name")
    return name


def matches(text: str, pattern: str, what: str = "") -> None:
    if re.search(pattern, text) is None:
        raise AssertionError(f"{what + ': ' if what else ''}{_shown(text)} does not match {pattern!r}")


@attrs.frozen
class Raw:
    """One answer the suite read without the SDK, for routes the SDK must never reach."""

    status: int
    body: str


def raw(method: str, path: str, key: str | None = None) -> Raw:
    """One request with the suite's own key, or the one given, so a refusal is the server's and not the SDK's."""
    ca_file = os.environ.get("SHARD_CA_FILE")
    verify: ssl.SSLContext | bool = ssl.create_default_context(cafile=ca_file) if ca_file else True
    token = key if key is not None else os.environ.get("SHARD_API_KEY", "")
    with httpx.Client(base_url=os.environ.get("SHARD_REMOTE", ""), verify=verify, timeout=60) as client:
        response = client.request(method, path, headers={"Authorization": f"Bearer {token}"})
        return Raw(status=response.status_code, body=response.text)


@attrs.frozen
class Block:
    stream: Stream
    tag: str
    size: int


@attrs.frozen
class Arrival:
    """One chunk a callback got, in the order the SDK got it."""

    stream: Stream
    data: bytes


def block_text(block: Block) -> bytes:
    """What `yes <tag> | head -c <size>` prints, so the suite knows every byte the writer sends."""
    line = f"{block.tag}\n".encode()
    return (line * -(-block.size // len(line)))[: block.size]


def writer(blocks: list[Block]) -> str:
    return "; ".join(
        f"yes {block.tag} | head -c {block.size}{' >&2' if block.stream == 'stderr' else ''}" for block in blocks
    )


def alternate(n: int, size: int) -> list[Block]:
    """n blocks of size bytes each, stdout first, so the two streams interleave."""
    blocks = []
    for i in range(n):
        stream: Stream = "stdout" if i % 2 == 0 else "stderr"
        blocks.append(Block(stream=stream, tag=f"{'o' if stream == 'stdout' else 'e'}{i}", size=size))
    return blocks


def expected(blocks: list[Block], stream: Stream) -> bytes:
    return b"".join(block_text(block) for block in blocks if block.stream == stream)


def joined(arrivals: list[Arrival], stream: Stream) -> bytes:
    return b"".join(each.data for each in arrivals if each.stream == stream)


def newest(arrivals: list[Arrival], limit: int) -> dict[Stream, bytes]:
    """The reference capture: the last limit bytes of the arrivals, both streams counted together."""
    kept: list[Arrival] = []
    left = limit
    for each in reversed(arrivals):
        if left == 0:
            break
        take = min(left, len(each.data))
        kept.append(Arrival(stream=each.stream, data=each.data[len(each.data) - take :]))
        left -= take
    kept.reverse()
    return {"stdout": joined(kept, "stdout"), "stderr": joined(kept, "stderr")}


@contextlib.contextmanager
def env(**values: str | None) -> Iterator[None]:
    """Run with the given variables in place of the real ones, None for unset, and put the real ones back."""
    saved = {key: os.environ.get(key) for key in values}
    _assign(values)
    try:
        yield
    finally:
        _assign(saved)


def _assign(values: dict[str, str | None]) -> None:
    for key, value in values.items():
        if value is None:
            os.environ.pop(key, None)
            continue
        os.environ[key] = value


def local_dir(prefix: str) -> Path:
    return Path(tempfile.mkdtemp(prefix=f"{prefix}-"))


def remove_local(path: Path) -> None:
    shutil.rmtree(path)


def write_local(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(data)


def read_local(path: Path) -> bytes:
    return path.read_bytes()


def tree(root: Path) -> dict[str, bytes]:
    """Every file under root and its bytes, keyed by the path relative to root."""
    return {path.relative_to(root).as_posix(): path.read_bytes() for path in sorted(root.rglob("*")) if path.is_file()}


@attrs.define
class Tally:
    count: int = 0
    passed: int = 0
    failed: int = 0
    skipped: int = 0

    def summary(self) -> str:
        return f"suite: {self.count} checks, {self.passed} passed, {self.failed} failed, {self.skipped} skipped"


def drain(strays: list[str]) -> list[str]:
    """Take every stray error so far; a collect first makes a dropped task report its error now, not later."""
    gc.collect()
    taken = list(strays)
    strays.clear()
    return taken


def failure_text(failure: BaseException | str) -> str:
    if isinstance(failure, str):
        return failure
    return "".join(traceback.format_exception(failure)).rstrip()


def first_line(failure: BaseException | str) -> str:
    text = failure if isinstance(failure, str) else describe(failure)
    return text.split("\n")[0]
